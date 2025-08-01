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

package secretstore

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	esapi "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	"github.com/external-secrets/external-secrets/pkg/controllers/secretstore/metrics"
)

const (
	errStoreClient         = "could not get provider client: %w"
	errValidationFailed    = "could not validate provider: %w"
	errPatchStatus         = "unable to patch status: %w"
	errUnableCreateClient  = "unable to create client"
	errUnableValidateStore = "unable to validate store: %s"

	msgStoreValidated     = "store validated"
	msgStoreNotMaintained = "store isn't currently maintained. Please plan and prepare accordingly."
)

type Opts struct {
	ControllerClass string
	GaugeVecGetter  metrics.GaugeVevGetter
	Recorder        record.EventRecorder
	RequeueInterval time.Duration
}

func reconcile(ctx context.Context, req ctrl.Request, ss esapi.GenericStore, cl client.Client, log logr.Logger, opts Opts) (ctrl.Result, error) {
	if !ShouldProcessStore(ss, opts.ControllerClass) {
		log.V(1).Info("skip store")
		return ctrl.Result{}, nil
	}

	requeueInterval := opts.RequeueInterval

	if ss.GetSpec().RefreshInterval != 0 {
		requeueInterval = time.Second * time.Duration(ss.GetSpec().RefreshInterval)
	}

	// patch status when done processing
	p := client.MergeFrom(ss.Copy())
	defer func() {
		err := cl.Status().Patch(ctx, ss, p)
		if err != nil {
			log.Error(err, errPatchStatus)
		}
	}()

	// validateStore modifies the store conditions
	// we have to patch the status
	log.V(1).Info("validating")
	err := validateStore(ctx, req.Namespace, opts.ControllerClass, ss, cl, opts.GaugeVecGetter, opts.Recorder)
	if err != nil {
		log.Error(err, "unable to validate store")
		return ctrl.Result{}, err
	}
	storeProvider, err := esapi.GetProvider(ss)
	if err != nil {
		return ctrl.Result{}, err
	}

	isMaintained, err := esapi.GetMaintenanceStatus(ss)
	if err != nil {
		return ctrl.Result{}, err
	}
	annotations := ss.GetAnnotations()
	_, ok := annotations["external-secrets.io/ignore-maintenance-checks"]

	if !bool(isMaintained) && !ok {
		opts.Recorder.Event(ss, v1.EventTypeWarning, esapi.StoreUnmaintained, msgStoreNotMaintained)
	}

	capStatus := esapi.SecretStoreStatus{
		Capabilities: storeProvider.Capabilities(),
		Conditions:   ss.GetStatus().Conditions,
	}
	ss.SetStatus(capStatus)

	opts.Recorder.Event(ss, v1.EventTypeNormal, esapi.ReasonStoreValid, msgStoreValidated)
	cond := NewSecretStoreCondition(esapi.SecretStoreReady, v1.ConditionTrue, esapi.ReasonStoreValid, msgStoreValidated)
	SetExternalSecretCondition(ss, *cond, opts.GaugeVecGetter)

	return ctrl.Result{
		RequeueAfter: requeueInterval,
	}, err
}

// validateStore tries to construct a new client
// if it fails sets a condition and writes events.
func validateStore(ctx context.Context, namespace, controllerClass string, store esapi.GenericStore,
	client client.Client, gaugeVecGetter metrics.GaugeVevGetter, recorder record.EventRecorder) error {
	mgr := NewManager(client, controllerClass, false)
	defer func() {
		_ = mgr.Close(ctx)
	}()
	cl, err := mgr.GetFromStore(ctx, store, namespace)
	if err != nil {
		cond := NewSecretStoreCondition(esapi.SecretStoreReady, v1.ConditionFalse, esapi.ReasonInvalidProviderConfig, errUnableCreateClient)
		SetExternalSecretCondition(store, *cond, gaugeVecGetter)
		recorder.Event(store, v1.EventTypeWarning, esapi.ReasonInvalidProviderConfig, err.Error())
		return fmt.Errorf(errStoreClient, err)
	}
	validationResult, err := cl.Validate()
	if err != nil && validationResult != esapi.ValidationResultUnknown {
		// Use ReasonInvalidProviderConfig for validation errors that indicate
		// invalid configuration (like empty server URL)
		reason := esapi.ReasonValidationFailed
		if validationResult == esapi.ValidationResultError {
			reason = esapi.ReasonInvalidProviderConfig
		}
		cond := NewSecretStoreCondition(esapi.SecretStoreReady, v1.ConditionFalse, reason, fmt.Sprintf(errUnableValidateStore, err))
		SetExternalSecretCondition(store, *cond, gaugeVecGetter)
		recorder.Event(store, v1.EventTypeWarning, reason, err.Error())
		return fmt.Errorf(errValidationFailed, err)
	}

	return nil
}

// ShouldProcessStore returns true if the store should be processed.
func ShouldProcessStore(store esapi.GenericStore, class string) bool {
	if store == nil || store.GetSpec().Controller == "" || store.GetSpec().Controller == class {
		return true
	}

	return false
}
