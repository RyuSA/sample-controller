/*
Copyright 2021.

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

package controllers

import (
	"context"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	samplecontrollerv1alpha1 "github.com/RyuSA/sample-controller/api/v1alpha1"
	"github.com/go-logr/logr"
)

var (
	deploymentOwnerKey = ".metadata.controller"
	apiGVStr           = samplecontrollerv1alpha1.GroupVersion.String()
)

// FooReconciler reconciles a Foo object
type FooReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=samplecontroller.ryusa.github.com,resources=foos,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=samplecontroller.ryusa.github.com,resources=foos/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *FooReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// your logic here

	log := r.Log.WithValues("foo", req.NamespacedName)

	var foo samplecontrollerv1alpha1.Foo
	log.Info("fetching,,,")

	// Golangのstructの埋め込みを利用している
	// FooReconcilerの実装ではなく、実態は埋め込んでいるclient.ClientのGetを呼んでいる
	// clientの内部キャッシュに含まれている情報を元にオブジェクトを取得してきている
	if err := r.Get(ctx, req.NamespacedName, &foo); err != nil {
		log.Error(err, "failed to fetching foo")
		// この時点でのerrはNotFoundエラーのはずで致命傷ではない
		var isNotFound = strconv.FormatBool(apierrors.IsNotFound(err))
		log.Info("Is this error NotFound?..." + isNotFound)
		// IgnoreNotFoundでエラーのラッパーを返却、最終的にエラーを握り潰す
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 古いDeploymentを管理外として削除する
	if err := r.cleanupOwnedResources(ctx, log, &foo); err != nil {
		log.Error(err, "failed to clean up old Deployment resources for this Foo")
		return ctrl.Result{}, err
	}

	// 新しいFooの`foo.spec.deploymentName`を参照する
	deploymentName := foo.Spec.DeploymentName
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: req.Namespace,
		},
	}

	// 新しいFooに合わせて、Deploymentがなければ作成、存在すれば更新をする
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		replicas := int32(1)
		if foo.Spec.Replicas != nil {
			replicas = *foo.Spec.Replicas
		}
		deploy.Spec.Replicas = &replicas

		labels := map[string]string{
			"app":        "nginx",
			"controller": req.Name,
		}

		// deployが新規作成の場合はSelectorが存在しないため定義する
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}

		// deployが新規作成の場合はlabelsが存在しないため定義する
		if deploy.Spec.Template.ObjectMeta.Labels == nil {
			deploy.Spec.Template.ObjectMeta.Labels = labels
		}

		containers := []corev1.Container{
			{
				Name:  "nginx",
				Image: "nginx:latest",
			},
		}

		// deployが新規作成の場合はコンテナが定義されていないため定義する
		if deploy.Spec.Template.Spec.Containers == nil {
			deploy.Spec.Template.Spec.Containers = containers
		}

		// deployに&fooを親を認識させる
		if err := ctrl.SetControllerReference(&foo, deploy, r.Scheme); err != nil {
			log.Error(err, "unable to set ownerReference from Foo to Deployment")
			return err
		}

		return nil

	}); err != nil {
		log.Error(err, "unable to ensure deployment is correct")
		return ctrl.Result{}, err
	}

	// 上記ctrl.CreateOrUpdateにて作成/更新されたDeploymentを取得する
	var deployment appsv1.Deployment
	var deploymentNamespecedName = client.ObjectKey{
		Namespace: req.Namespace,
		Name:      foo.Spec.DeploymentName,
	}

	if err := r.Get(ctx, deploymentNamespecedName, &deployment); err != nil {
		log.Error(err, "unable to fetch Deployment")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	availableReplicas := deployment.Status.AvailableReplicas
	if availableReplicas == foo.Status.AvailableReplicas {
		// same status, done
		return ctrl.Result{}, nil
	}

	foo.Status.AvailableReplicas = availableReplicas

	// 作成したDeploymentベースにFooステータスを更新する
	if err := r.Status().Update(ctx, &foo); err != nil {
		log.Error(err, "Failed to update Foo status")
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(&foo, corev1.EventTypeNormal, "Updated", "Update foo.status.AvairableReplicas: %d", foo.Status.AvailableReplicas)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FooReconciler) SetupWithManager(mgr ctrl.Manager) error {

	if err := mgr.GetFieldIndexer().IndexField(
		// 書籍のバージョンで動作しないため修正
		// rootのcontext?から空のcontextを取得してくる
		context.Background(),
		&appsv1.Deployment{},
		deploymentOwnerKey,
		// 書籍のバージョンで動作しないため修正
		// IndexFieldの第4引数のインタフェースIndexerFuncを見るに、この関数で引き取る方はruntime.Objectではなくclient.Object
		func(raw client.Object) []string {
			deployment := raw.(*appsv1.Deployment)
			owner := metav1.GetControllerOf(deployment)
			if owner == nil {
				return nil
			}
			if owner.APIVersion != apiGVStr || owner.Kind != "Foo" {
				return nil
			}
			return []string{owner.Name}
		}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&samplecontrollerv1alpha1.Foo{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func (r *FooReconciler) cleanupOwnedResources(ctx context.Context, log logr.Logger, foo *samplecontrollerv1alpha1.Foo) error {
	log.Info("finding existing Deployments for Foo")
	var deployments appsv1.DeploymentList
	if err := r.List(
		ctx,
		&deployments,
		client.InNamespace(foo.Namespace),
		client.MatchingFields(map[string]string{deploymentOwnerKey: foo.Name}),
	); err != nil {
		return err
	}

	for _, deployment := range deployments.Items {
		if deployment.Name == foo.Spec.DeploymentName {
			continue
		}

		if err := r.Delete(ctx, &deployment); err != nil {
			log.Error(err, "Failed to delete Deployment"+deployment.Name)
			return err
		}

		log.Info("Deployment " + deployment.Name + " has been deleted")
		r.Recorder.Eventf(foo, corev1.EventTypeNormal, "Deleted", "Deleted Deployment %q", deployment.Name)
	}

	return nil
}
