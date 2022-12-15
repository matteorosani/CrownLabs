// Copyright 2020-2022 Politecnico di Torino
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package instctrl

import (
	"context"
	"errors"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/netgroup-polito/CrownLabs/operators/api/v1alpha2"
	clctx "github.com/netgroup-polito/CrownLabs/operators/pkg/context"
	"github.com/netgroup-polito/CrownLabs/operators/pkg/forge"
	"github.com/netgroup-polito/CrownLabs/operators/pkg/utils"
)

// Enforce the setting for the NFS mounting path on the container
// The function is called in function EnforceContainerEnvironment
func (r *InstanceReconciler) EnforceNFSMount(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)
	var retErr error

	// Retrieve the correct Tenant from the given context
	tenant := clctx.TenantFrom(ctx)
	if tenant == nil {
		retErr = errors.New("unable to retrieve tenant from the context")
		klog.Errorf("%s", retErr)
		return retErr
	}

	if tenant.Status.PersonalNamespace.Created {
		klog.Infof("Tenant Namespace %s", tenant.Status.PersonalNamespace.Name)

		// Get the secret and the NFS path
		secret := v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "user-pvc-secret", Namespace: tenant.Status.PersonalNamespace.Name}}
		klog.Infof("Secret %s %s", secret.Name, secret.Namespace)
		if retErr = r.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, &secret); retErr != nil {
			klog.Errorf("Unable to get secret for tenant %s in namespace %s. Error %s", tenant.Name, tenant.Status.PersonalNamespace.Name, retErr)
			return retErr
		} else {
			var share []byte
			var ok bool
			if share, ok = secret.Data["share"]; !ok {
				retErr = errors.New("unable to retrieve user path")
				klog.Errorf("Unable to get secret for tenant ", tenant.Name, retErr)
				return retErr
			}
			// Store the user path obtained through the secret
			shareString := string(share)

			// Retrieve the public keys
			publicKeys, err := r.GetPublicKeys(ctx)
			if err != nil {
				log.Error(err, "unable to get public keys")
				return err
			}
			log.V(utils.LogDebugLevel).Info("public keys correctly retrieved")

			// Add the mounting path for the user in the container data
			// See the CloudInitUserDataContainer function in /forge/cloudinit.go
			_, err = forge.CloudInitUserDataContainer(shareString, publicKeys)
			if retErr != nil {
				log.Error(err, "unable to marshal secret content")
				return err
			}
		}
	}

	return nil
}

// EnforceContainerEnvironment implements the logic to create all the different
// Kubernetes resources required to start a containerized CrownLabs environment.
func (r *InstanceReconciler) EnforceContainerEnvironment(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)
	environment := clctx.EnvironmentFrom(ctx)

	// Enforce the service and the ingress to expose the environment.
	if err := r.EnforceInstanceExposition(ctx); err != nil {
		log.Error(err, "failed to enforce the instance exposition objects")
		return err
	}

	// Enforce the NFS user mount path
	if err := r.EnforceNFSMount(ctx); err != nil {
		log.Error(err, "failed to enforce the NFS mounting")
		return err
	}

	// Enforce the environment's PVC in case of a persistent environment
	if environment.Persistent {
		if err := r.enforcePVC(ctx); err != nil {
			return err
		}
	}

	return r.enforceContainer(ctx)
}

// enforcePVC enforces the presence of the instance persistent storage
// which consists in a PVC.
func (r *InstanceReconciler) enforcePVC(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)
	instance := clctx.InstanceFrom(ctx)
	environment := clctx.EnvironmentFrom(ctx)

	pvc := v1.PersistentVolumeClaim{ObjectMeta: forge.ObjectMeta(instance)}

	res, err := ctrl.CreateOrUpdate(ctx, r.Client, &pvc, func() error {
		// PVC's spec is immutable, it has to be set at creation
		if pvc.ObjectMeta.CreationTimestamp.IsZero() {
			pvc.Spec = forge.PVCSpec(environment)
		}
		pvc.SetLabels(forge.InstanceObjectLabels(pvc.GetLabels(), instance))
		return ctrl.SetControllerReference(instance, &pvc, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to enforce object", "pvc", klog.KObj(&pvc))
		return err
	}
	log.V(utils.FromResult(res)).Info("object enforced", "pvc", klog.KObj(&pvc), "result", res)
	return nil
}

// enforceContainer enforces the actual deployment
// which contains all the container based instance components.
func (r *InstanceReconciler) enforceContainer(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)
	instance := clctx.InstanceFrom(ctx)
	environment := clctx.EnvironmentFrom(ctx)

	depl := appsv1.Deployment{ObjectMeta: forge.ObjectMeta(instance)}

	res, err := ctrl.CreateOrUpdate(ctx, r.Client, &depl, func() error {
		// Deployment specifications are forged only at creation time, as changing them later may be
		// either rejected or cause the restart of the Pod, with consequent possible data loss.
		if depl.CreationTimestamp.IsZero() {
			depl.Spec = forge.DeploymentSpec(instance, environment, &r.ContainerEnvOpts)
		}

		depl.Spec.Replicas = forge.ReplicasCount(instance, environment, depl.CreationTimestamp.IsZero())

		depl.SetLabels(forge.InstanceObjectLabels(depl.GetLabels(), instance))
		return ctrl.SetControllerReference(instance, &depl, r.Scheme)
	})

	if err != nil {
		log.Error(err, "failed to enforce deployment existence", "deployment", klog.KObj(&depl))
		return err
	}

	log.V(utils.FromResult(res)).Info("object enforced", "deployment", klog.KObj(&depl), "result", res)

	phase := r.RetrievePhaseFromDeployment(&depl)

	// in case of non-running, non-persistent instances, just exposition is teared down
	// we consider them off even if the container is still on, to avoid data loss
	if !instance.Spec.Running && !environment.Persistent {
		phase = v1alpha2.EnvironmentPhaseOff
	}

	if phase != instance.Status.Phase {
		log.Info("phase changed", "deployment", klog.KObj(&depl),
			"previous", string(instance.Status.Phase), "current", string(phase))
		instance.Status.Phase = phase
	}

	return nil
}
