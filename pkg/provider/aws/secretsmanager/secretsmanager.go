/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package secretsmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilpointer "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	esv1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	"github.com/external-secrets/external-secrets/pkg/constants"
	"github.com/external-secrets/external-secrets/pkg/find"
	"github.com/external-secrets/external-secrets/pkg/metrics"
	"github.com/external-secrets/external-secrets/pkg/provider/aws/util"
	"github.com/external-secrets/external-secrets/pkg/utils"
	"github.com/external-secrets/external-secrets/pkg/utils/metadata"
)

type PushSecretMetadataSpec struct {
	Tags             map[string]string `json:"tags,omitempty"`
	Description      string            `json:"description,omitempty"`
	SecretPushFormat string            `json:"secretPushFormat,omitempty"`
	KMSKeyID         string            `json:"kmsKeyId,omitempty"`
}

// Declares metadata information for pushing secrets to AWS Secret Store.
const (
	SecretPushFormatKey       = "secretPushFormat"
	SecretPushFormatString    = "string"
	SecretPushFormatBinary    = "binary"
	ResourceNotFoundException = "ResourceNotFoundException"
)

// https://github.com/external-secrets/external-secrets/issues/644
var _ esv1.SecretsClient = &SecretsManager{}

// SecretsManager is a provider for AWS SecretsManager.
type SecretsManager struct {
	cfg          *aws.Config
	client       SMInterface // Keep the interface
	referentAuth bool
	cache        map[string]*awssm.GetSecretValueOutput
	config       *esv1.SecretsManager
	prefix       string
}

// SMInterface is a subset of the smiface api.
// see: https://docs.aws.amazon.com/sdk-for-go/api/service/secretsmanager/secretsmanageriface/
type SMInterface interface {
	BatchGetSecretValue(ctx context.Context, params *awssm.BatchGetSecretValueInput, optFuncs ...func(*awssm.Options)) (*awssm.BatchGetSecretValueOutput, error)
	ListSecrets(ctx context.Context, params *awssm.ListSecretsInput, optFuncs ...func(*awssm.Options)) (*awssm.ListSecretsOutput, error)
	GetSecretValue(ctx context.Context, params *awssm.GetSecretValueInput, optFuncs ...func(*awssm.Options)) (*awssm.GetSecretValueOutput, error)
	CreateSecret(ctx context.Context, params *awssm.CreateSecretInput, optFuncs ...func(*awssm.Options)) (*awssm.CreateSecretOutput, error)
	PutSecretValue(ctx context.Context, params *awssm.PutSecretValueInput, optFuncs ...func(*awssm.Options)) (*awssm.PutSecretValueOutput, error)
	DescribeSecret(ctx context.Context, params *awssm.DescribeSecretInput, optFuncs ...func(*awssm.Options)) (*awssm.DescribeSecretOutput, error)
	DeleteSecret(ctx context.Context, params *awssm.DeleteSecretInput, optFuncs ...func(*awssm.Options)) (*awssm.DeleteSecretOutput, error)
	TagResource(ctx context.Context, params *awssm.TagResourceInput, optFuncs ...func(*awssm.Options)) (*awssm.TagResourceOutput, error)
	UntagResource(ctx context.Context, params *awssm.UntagResourceInput, optFuncs ...func(*awssm.Options)) (*awssm.UntagResourceOutput, error)
}

const (
	errUnexpectedFindOperator = "unexpected find operator"
	managedBy                 = "managed-by"
	externalSecrets           = "external-secrets"
	initialVersion            = "00000000-0000-0000-0000-000000000001"
)

var log = ctrl.Log.WithName("provider").WithName("aws").WithName("secretsmanager")

// New creates a new SecretsManager client.
func New(_ context.Context, cfg *aws.Config, secretsManagerCfg *esv1.SecretsManager, prefix string, referentAuth bool) (*SecretsManager, error) {
	return &SecretsManager{
		cfg: cfg,
		client: awssm.NewFromConfig(*cfg, func(o *awssm.Options) {
			o.EndpointResolverV2 = customEndpointResolver{}
		}),
		referentAuth: referentAuth,
		cache:        make(map[string]*awssm.GetSecretValueOutput),
		config:       secretsManagerCfg,
		prefix:       prefix,
	}, nil
}

func (sm *SecretsManager) fetch(ctx context.Context, ref esv1.ExternalSecretDataRemoteRef) (*awssm.GetSecretValueOutput, error) {
	ver := "AWSCURRENT"
	valueFrom := "SECRET"
	if ref.Version != "" {
		ver = ref.Version
	}
	if ref.MetadataPolicy == esv1.ExternalSecretMetadataPolicyFetch {
		valueFrom = "TAG"
	}

	key := sm.prefix + ref.Key
	log.Info("fetching secret value", "key", key, "version", ver, "value", valueFrom)

	cacheKey := fmt.Sprintf("%s#%s#%s", key, ver, valueFrom)
	if secretOut, found := sm.cache[cacheKey]; found {
		log.Info("found secret in cache", "key", key, "version", ver)
		return secretOut, nil
	}

	secretOut, err := sm.constructSecretValue(ctx, key, ver, ref.MetadataPolicy)
	if err != nil {
		return nil, err
	}

	sm.cache[cacheKey] = secretOut

	return secretOut, nil
}

func (sm *SecretsManager) DeleteSecret(ctx context.Context, remoteRef esv1.PushSecretRemoteRef) error {
	secretName := sm.prefix + remoteRef.GetRemoteKey()
	secretValue := awssm.GetSecretValueInput{
		SecretId: &secretName,
	}
	secretInput := awssm.DescribeSecretInput{
		SecretId: &secretName,
	}
	awsSecret, err := sm.client.GetSecretValue(ctx, &secretValue)
	metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMGetSecretValue, err)
	var aerr smithy.APIError
	if err != nil {
		if ok := errors.As(err, &aerr); !ok {
			return err
		}
		if aerr.ErrorCode() == ResourceNotFoundException {
			return nil
		}
		return err
	}
	data, err := sm.client.DescribeSecret(ctx, &secretInput)
	metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMDescribeSecret, err)
	if err != nil {
		return err
	}
	if !isManagedByESO(data) {
		return nil
	}
	deleteInput := &awssm.DeleteSecretInput{
		SecretId: awsSecret.ARN,
	}
	if sm.config != nil && sm.config.ForceDeleteWithoutRecovery {
		deleteInput.ForceDeleteWithoutRecovery = &sm.config.ForceDeleteWithoutRecovery
	}
	if sm.config != nil && sm.config.RecoveryWindowInDays > 0 {
		deleteInput.RecoveryWindowInDays = &sm.config.RecoveryWindowInDays
	}
	err = util.ValidateDeleteSecretInput(*deleteInput)
	if err != nil {
		return err
	}
	_, err = sm.client.DeleteSecret(ctx, deleteInput)
	metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMDeleteSecret, err)
	return err
}

func (sm *SecretsManager) SecretExists(ctx context.Context, pushSecretRef esv1.PushSecretRemoteRef) (bool, error) {
	secretName := sm.prefix + pushSecretRef.GetRemoteKey()
	secretValue := awssm.GetSecretValueInput{
		SecretId: &secretName,
	}
	_, err := sm.client.GetSecretValue(ctx, &secretValue)
	if err != nil {
		return sm.handleSecretError(err)
	}
	return true, nil
}

func (sm *SecretsManager) handleSecretError(err error) (bool, error) {
	var aerr smithy.APIError
	if ok := errors.As(err, &aerr); !ok {
		return false, err
	}
	if aerr.ErrorCode() == ResourceNotFoundException {
		return false, nil
	}
	return false, err
}

func (sm *SecretsManager) PushSecret(ctx context.Context, secret *corev1.Secret, psd esv1.PushSecretData) error {
	value, err := utils.ExtractSecretData(psd, secret)
	if err != nil {
		return fmt.Errorf("failed to extract secret data: %w", err)
	}

	secretName := sm.prefix + psd.GetRemoteKey()
	secretValue := awssm.GetSecretValueInput{
		SecretId: &secretName,
	}

	secretInput := awssm.DescribeSecretInput{
		SecretId: &secretName,
	}

	awsSecret, err := sm.client.GetSecretValue(ctx, &secretValue)
	metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMGetSecretValue, err)

	if psd.GetProperty() != "" {
		currentSecret := sm.retrievePayload(awsSecret)
		if currentSecret != "" && !gjson.Valid(currentSecret) {
			return errors.New("PushSecret for aws secrets manager with a pushSecretData property requires a json secret")
		}
		value, _ = sjson.SetBytes([]byte(currentSecret), psd.GetProperty(), value)
	}

	var aerr smithy.APIError
	if err != nil {
		if ok := errors.As(err, &aerr); !ok {
			return err
		}

		if aerr.ErrorCode() == ResourceNotFoundException {
			return sm.createSecretWithContext(ctx, secretName, psd, value)
		}

		return err
	}

	return sm.putSecretValueWithContext(ctx, secretInput, awsSecret, psd, value)
}

func padOrTrim(b []byte) []byte {
	l := len(b)
	size := 16
	if l == size {
		return b
	}
	if l > size {
		return b[l-size:]
	}
	tmp := make([]byte, size)
	copy(tmp[size-l:], b)
	return tmp
}

func bumpVersionNumber(id *string) (*string, error) {
	if id == nil {
		output := initialVersion
		return &output, nil
	}
	n := new(big.Int)
	oldVersion, ok := n.SetString(strings.ReplaceAll(*id, "-", ""), 16)
	if !ok {
		return nil, fmt.Errorf("expected secret version in AWS SSM to be a UUID but got '%s'", *id)
	}
	newVersionRaw := oldVersion.Add(oldVersion, big.NewInt(1)).Bytes()

	newVersion, err := uuid.FromBytes(padOrTrim(newVersionRaw))
	if err != nil {
		return nil, err
	}

	s := newVersion.String()
	return &s, nil
}

func isManagedByESO(data *awssm.DescribeSecretOutput) bool {
	managedBy := managedBy
	externalSecrets := externalSecrets
	for _, tag := range data.Tags {
		if *tag.Key == managedBy && *tag.Value == externalSecrets {
			return true
		}
	}
	return false
}

// GetAllSecrets syncs multiple secrets from aws provider into a single Kubernetes Secret.
func (sm *SecretsManager) GetAllSecrets(ctx context.Context, ref esv1.ExternalSecretFind) (map[string][]byte, error) {
	if ref.Name != nil {
		return sm.findByName(ctx, ref)
	}
	if len(ref.Tags) > 0 {
		return sm.findByTags(ctx, ref)
	}
	return nil, errors.New(errUnexpectedFindOperator)
}

func (sm *SecretsManager) findByName(ctx context.Context, ref esv1.ExternalSecretFind) (map[string][]byte, error) {
	matcher, err := find.New(*ref.Name)
	if err != nil {
		return nil, err
	}

	filters := make([]types.Filter, 0)
	if ref.Path != nil {
		filters = append(filters, types.Filter{
			Key: types.FilterNameStringTypeName,
			Values: []string{
				*ref.Path,
			},
		})

		return sm.fetchWithBatch(ctx, filters, matcher)
	}

	data := make(map[string][]byte)
	var nextToken *string

	for {
		// I put this into the for loop on purpose.
		log.V(0).Info("using ListSecret to fetch all secrets; this is a costly operations, please use batching by defining a _path_")
		it, err := sm.client.ListSecrets(ctx, &awssm.ListSecretsInput{
			Filters:   filters,
			NextToken: nextToken,
		})
		metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMListSecrets, err)
		if err != nil {
			return nil, err
		}
		log.V(1).Info("aws sm findByName found", "secrets", len(it.SecretList))
		for _, secret := range it.SecretList {
			if !matcher.MatchName(*secret.Name) {
				continue
			}
			log.V(1).Info("aws sm findByName matches", "name", *secret.Name)
			if err := sm.fetchAndSet(ctx, data, *secret.Name); err != nil {
				return nil, err
			}
		}
		nextToken = it.NextToken
		if nextToken == nil {
			break
		}
	}
	return data, nil
}

func (sm *SecretsManager) findByTags(ctx context.Context, ref esv1.ExternalSecretFind) (map[string][]byte, error) {
	filters := make([]types.Filter, 0)
	for k, v := range ref.Tags {
		filters = append(filters, types.Filter{
			Key: types.FilterNameStringTypeTagKey,
			Values: []string{
				k,
			},
		}, types.Filter{
			Key: types.FilterNameStringTypeTagValue,
			Values: []string{
				v,
			},
		})
	}

	if ref.Path != nil {
		filters = append(filters, types.Filter{
			Key: types.FilterNameStringTypeName,
			Values: []string{
				*ref.Path,
			},
		})
	}

	return sm.fetchWithBatch(ctx, filters, nil)
}

func (sm *SecretsManager) fetchAndSet(ctx context.Context, data map[string][]byte, name string) error {
	sec, err := sm.fetch(ctx, esv1.ExternalSecretDataRemoteRef{
		Key: name,
	})
	if err != nil {
		return err
	}
	if sec.SecretString != nil {
		data[name] = []byte(*sec.SecretString)
	}
	if sec.SecretBinary != nil {
		data[name] = sec.SecretBinary
	}
	return nil
}

// GetSecret returns a single secret from the provider.
func (sm *SecretsManager) GetSecret(ctx context.Context, ref esv1.ExternalSecretDataRemoteRef) ([]byte, error) {
	secretOut, err := sm.fetch(ctx, ref)
	if errors.Is(err, esv1.NoSecretErr) {
		return nil, err
	}
	if err != nil {
		return nil, util.SanitizeErr(err)
	}
	if ref.Property == "" {
		if secretOut.SecretString != nil {
			return []byte(*secretOut.SecretString), nil
		}
		if secretOut.SecretBinary != nil {
			return secretOut.SecretBinary, nil
		}
		return nil, fmt.Errorf("invalid secret received. no secret string nor binary for key: %s", ref.Key)
	}
	val := sm.mapSecretToGjson(secretOut, ref.Property)
	if !val.Exists() {
		return nil, fmt.Errorf("key %s does not exist in secret %s", ref.Property, ref.Key)
	}
	return []byte(val.String()), nil
}

func (sm *SecretsManager) mapSecretToGjson(secretOut *awssm.GetSecretValueOutput, property string) gjson.Result {
	payload := sm.retrievePayload(secretOut)
	refProperty := sm.escapeDotsIfRequired(property, payload)
	val := gjson.Get(payload, refProperty)
	return val
}

func (sm *SecretsManager) retrievePayload(secretOut *awssm.GetSecretValueOutput) string {
	if secretOut == nil {
		return ""
	}

	var payload string
	if secretOut.SecretString != nil {
		payload = *secretOut.SecretString
	}
	if secretOut.SecretBinary != nil {
		payload = string(secretOut.SecretBinary)
	}
	return payload
}

func (sm *SecretsManager) escapeDotsIfRequired(currentRefProperty, payload string) string {
	// We need to search if a given key with a . exists before using gjson operations.
	idx := strings.Index(currentRefProperty, ".")
	refProperty := currentRefProperty
	if idx > -1 {
		refProperty = strings.ReplaceAll(currentRefProperty, ".", "\\.")
		val := gjson.Get(payload, refProperty)
		if !val.Exists() {
			refProperty = currentRefProperty
		}
	}
	return refProperty
}

// GetSecretMap returns multiple k/v pairs from the provider.
func (sm *SecretsManager) GetSecretMap(ctx context.Context, ref esv1.ExternalSecretDataRemoteRef) (map[string][]byte, error) {
	log.Info("fetching secret map", "key", ref.Key)
	data, err := sm.GetSecret(ctx, ref)
	if err != nil {
		return nil, err
	}
	kv := make(map[string]json.RawMessage)
	err = json.Unmarshal(data, &kv)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal secret %s: %w", ref.Key, err)
	}
	secretData := make(map[string][]byte)
	for k, v := range kv {
		var strVal string
		err = json.Unmarshal(v, &strVal)
		if err == nil {
			secretData[k] = []byte(strVal)
		} else {
			secretData[k] = v
		}
	}
	return secretData, nil
}

func (sm *SecretsManager) Close(_ context.Context) error {
	return nil
}

func (sm *SecretsManager) Validate() (esv1.ValidationResult, error) {
	// skip validation stack because it depends on the namespace
	// of the ExternalSecret
	if sm.referentAuth {
		return esv1.ValidationResultUnknown, nil
	}
	_, err := sm.cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		return esv1.ValidationResultError, util.SanitizeErr(err)
	}

	return esv1.ValidationResultReady, nil
}

func (sm *SecretsManager) Capabilities() esv1.SecretStoreCapabilities {
	return esv1.SecretStoreReadWrite
}

func (sm *SecretsManager) createSecretWithContext(ctx context.Context, secretName string, psd esv1.PushSecretData, value []byte) error {
	mdata, err := sm.constructMetadataWithDefaults(psd.GetMetadata())
	if err != nil {
		return fmt.Errorf("failed to parse push secret metadata: %w", err)
	}

	tags := make([]types.Tag, 0)

	for k, v := range mdata.Spec.Tags {
		tags = append(tags, types.Tag{
			Key:   utilpointer.To(k),
			Value: utilpointer.To(v),
		})
	}

	input := &awssm.CreateSecretInput{
		Name:               &secretName,
		SecretBinary:       value,
		Tags:               tags,
		Description:        utilpointer.To(mdata.Spec.Description),
		ClientRequestToken: utilpointer.To(initialVersion),
		KmsKeyId:           utilpointer.To(mdata.Spec.KMSKeyID),
	}
	if mdata.Spec.SecretPushFormat == SecretPushFormatString {
		input.SecretBinary = nil
		input.SecretString = aws.String(string(value))
	}

	_, err = sm.client.CreateSecret(ctx, input)
	metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMCreateSecret, err)

	return err
}

func (sm *SecretsManager) putSecretValueWithContext(ctx context.Context, secretInput awssm.DescribeSecretInput, awsSecret *awssm.GetSecretValueOutput, psd esv1.PushSecretData, value []byte) error {
	data, err := sm.client.DescribeSecret(ctx, &secretInput)
	metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMDescribeSecret, err)
	if err != nil {
		return err
	}
	if !isManagedByESO(data) {
		return errors.New("secret not managed by external-secrets")
	}
	if awsSecret != nil && bytes.Equal(awsSecret.SecretBinary, value) || utils.CompareStringAndByteSlices(awsSecret.SecretString, value) {
		return nil
	}

	newVersionNumber, err := bumpVersionNumber(awsSecret.VersionId)
	if err != nil {
		return err
	}
	input := &awssm.PutSecretValueInput{
		SecretId:           awsSecret.ARN,
		SecretBinary:       value,
		ClientRequestToken: newVersionNumber,
	}
	secretPushFormat, err := utils.FetchValueFromMetadata(SecretPushFormatKey, psd.GetMetadata(), SecretPushFormatBinary)
	if err != nil {
		return fmt.Errorf("failed to parse metadata: %w", err)
	}
	if secretPushFormat == SecretPushFormatString {
		input.SecretBinary = nil
		input.SecretString = aws.String(string(value))
	}

	_, err = sm.client.PutSecretValue(ctx, input)
	metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMPutSecretValue, err)
	if err != nil {
		return err
	}

	currentTags := make(map[string]string, len(data.Tags))
	for _, tag := range data.Tags {
		currentTags[*tag.Key] = *tag.Value
	}
	return sm.patchTags(ctx, psd.GetMetadata(), awsSecret.ARN, currentTags)
}

func (sm *SecretsManager) patchTags(ctx context.Context, metadata *apiextensionsv1.JSON, secretId *string, tags map[string]string) error {
	meta, err := sm.constructMetadataWithDefaults(metadata)
	if err != nil {
		return err
	}

	tagKeysToRemove := util.FindTagKeysToRemove(tags, meta.Spec.Tags)
	if len(tagKeysToRemove) > 0 {
		_, err = sm.client.UntagResource(ctx, &awssm.UntagResourceInput{
			SecretId: secretId,
			TagKeys:  tagKeysToRemove,
		})
		metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMUntagResource, err)
		if err != nil {
			return err
		}
	}

	tagsToUpdate, isModified := computeTagsToUpdate(tags, meta.Spec.Tags)
	if isModified {
		_, err = sm.client.TagResource(ctx, &awssm.TagResourceInput{
			SecretId: secretId,
			Tags:     tagsToUpdate,
		})
		metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMTagResource, err)
		if err != nil {
			return err
		}
	}
	return nil
}

func (sm *SecretsManager) fetchWithBatch(ctx context.Context, filters []types.Filter, matcher *find.Matcher) (map[string][]byte, error) {
	data := make(map[string][]byte)
	var nextToken *string

	for {
		it, err := sm.client.BatchGetSecretValue(ctx, &awssm.BatchGetSecretValueInput{
			Filters:   filters,
			NextToken: nextToken,
		})
		metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMBatchGetSecretValue, err)
		if err != nil {
			return nil, err
		}
		log.V(1).Info("aws sm findByName found", "secrets", len(it.SecretValues))
		for _, secret := range it.SecretValues {
			if matcher != nil && !matcher.MatchName(*secret.Name) {
				continue
			}
			log.V(1).Info("aws sm findByName matches", "name", *secret.Name)

			sm.setSecretValues(&secret, data)
		}
		nextToken = it.NextToken
		if nextToken == nil {
			break
		}
	}

	return data, nil
}

func (sm *SecretsManager) setSecretValues(secret *types.SecretValueEntry, data map[string][]byte) {
	if secret.SecretString != nil {
		data[*secret.Name] = []byte(*secret.SecretString)
	}
	if secret.SecretBinary != nil {
		data[*secret.Name] = secret.SecretBinary
	}
}

func (sm *SecretsManager) constructSecretValue(ctx context.Context, key, ver string, metadataPolicy esv1.ExternalSecretMetadataPolicy) (*awssm.GetSecretValueOutput, error) {
	if metadataPolicy == esv1.ExternalSecretMetadataPolicyFetch {
		describeSecretInput := &awssm.DescribeSecretInput{
			SecretId: &key,
		}

		descOutput, err := sm.client.DescribeSecret(ctx, describeSecretInput)
		if err != nil {
			return nil, err
		}
		log.Info("found metadata secret", "key", key, "output", descOutput)

		jsonTags, err := util.SecretTagsToJSONString(descOutput.Tags)
		if err != nil {
			return nil, err
		}
		return &awssm.GetSecretValueOutput{
			ARN:          descOutput.ARN,
			CreatedDate:  descOutput.CreatedDate,
			Name:         descOutput.Name,
			SecretString: &jsonTags,
			VersionId:    &ver,
		}, nil
	}

	var getSecretValueInput *awssm.GetSecretValueInput
	if strings.HasPrefix(ver, "uuid/") {
		versionID := strings.TrimPrefix(ver, "uuid/")
		getSecretValueInput = &awssm.GetSecretValueInput{
			SecretId:  &key,
			VersionId: &versionID,
		}
	} else {
		getSecretValueInput = &awssm.GetSecretValueInput{
			SecretId:     &key,
			VersionStage: &ver,
		}
	}
	secretOut, err := sm.client.GetSecretValue(ctx, getSecretValueInput)
	metrics.ObserveAPICall(constants.ProviderAWSSM, constants.CallAWSSMGetSecretValue, err)
	var (
		nf *types.ResourceNotFoundException
		ie *types.InvalidParameterException
	)
	if errors.As(err, &nf) {
		return nil, esv1.NoSecretErr
	}

	if errors.As(err, &ie) && strings.Contains(ie.Error(), "was marked for deletion") {
		return nil, esv1.NoSecretErr
	}

	return secretOut, err
}

func (sm *SecretsManager) constructMetadataWithDefaults(data *apiextensionsv1.JSON) (*metadata.PushSecretMetadata[PushSecretMetadataSpec], error) {
	var (
		meta *metadata.PushSecretMetadata[PushSecretMetadataSpec]
		err  error
	)

	meta, err = metadata.ParseMetadataParameters[PushSecretMetadataSpec](data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	if meta == nil {
		meta = &metadata.PushSecretMetadata[PushSecretMetadataSpec]{}
	}

	if meta.Spec.SecretPushFormat == "" {
		meta.Spec.SecretPushFormat = SecretPushFormatBinary
	} else if !slices.Contains([]string{SecretPushFormatBinary, SecretPushFormatString}, meta.Spec.SecretPushFormat) {
		return nil, fmt.Errorf("invalid secret push format: %s", meta.Spec.SecretPushFormat)
	}

	if meta.Spec.Description == "" {
		meta.Spec.Description = fmt.Sprintf("secret '%s:%s'", managedBy, externalSecrets)
	}

	if meta.Spec.KMSKeyID == "" {
		meta.Spec.KMSKeyID = "alias/aws/secretsmanager"
	}

	if len(meta.Spec.Tags) > 0 {
		if _, exists := meta.Spec.Tags[managedBy]; exists {
			return nil, fmt.Errorf("error parsing tags in metadata: Cannot specify a '%s' tag", managedBy)
		}
	} else {
		meta.Spec.Tags = make(map[string]string)
	}
	meta.Spec.Tags[managedBy] = externalSecrets

	return meta, nil
}

// computeTagsToUpdate compares the current tags with the desired metaTags and returns a slice of ssmTypes.Tag
// that should be set on the resource. It also returns a boolean indicating if any tag was added or modified.
func computeTagsToUpdate(tags, metaTags map[string]string) ([]types.Tag, bool) {
	result := make([]types.Tag, 0, len(metaTags))
	modified := false
	for k, v := range metaTags {
		if _, exists := tags[k]; !exists || tags[k] != v {
			if k != managedBy {
				modified = true
			}
		}
		result = append(result, types.Tag{
			Key:   utilpointer.To(k),
			Value: utilpointer.To(v),
		})
	}
	return result, modified
}
