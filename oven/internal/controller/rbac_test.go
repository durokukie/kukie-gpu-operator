/*
Copyright 2026.

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

package controller

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

// Every spec takes a fresh namespace of its own, but the cache needs its namespaces before the manager starts. So the
// suite makes this many up front and hands them out in order. Raise it when the specs outgrow it.
const watchedNamespaceCount = 100

var (
	watchedNamespaces []string
	namespacesHandOut int
	// ovenConfig connects as the controller's user, which has only the generated rules.
	ovenConfig *rest.Config
)

// nextWatchedNamespace returns a namespace the controller watches and has its Role in, unused by any earlier spec.
func nextWatchedNamespace() string {
	if namespacesHandOut >= len(watchedNamespaces) {
		Fail(fmt.Sprintf("all %d watched namespaces are used; raise watchedNamespaceCount", len(watchedNamespaces)))
	}
	ns := watchedNamespaces[namespacesHandOut]
	namespacesHandOut++
	return ns
}

// generatedRoles reads the ClusterRole and the Role controller-gen writes from the +kubebuilder:rbac markers.
func generatedRoles() (*rbacv1.ClusterRole, *rbacv1.Role) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var clusterRole *rbacv1.ClusterRole
	var role *rbacv1.Role
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		var meta metav1.TypeMeta
		ExpectWithOffset(1, yaml.Unmarshal(doc, &meta)).To(Succeed())
		switch meta.Kind {
		case "ClusterRole":
			clusterRole = &rbacv1.ClusterRole{}
			ExpectWithOffset(1, yaml.Unmarshal(doc, clusterRole)).To(Succeed())
		case "Role":
			role = &rbacv1.Role{}
			ExpectWithOffset(1, yaml.Unmarshal(doc, role)).To(Succeed())
		}
	}
	ExpectWithOffset(1, clusterRole).NotTo(BeNil(), "role.yaml has no ClusterRole")
	ExpectWithOffset(1, role).NotTo(BeNil(), "role.yaml has no Role")
	return clusterRole, role
}

// startAsOven makes the namespaces, grants the generated rules to a user of its own and returns that user's config.
func startAsOven(ctx context.Context, namespaces []string) *rest.Config {
	const user = "kukie-oven"
	clusterRole, role := generatedRoles()
	subjects := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}

	Expect(k8sClient.Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: user}, Rules: clusterRole.Rules})).To(Succeed())
	Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: user},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: user},
		Subjects:   subjects,
	})).To(Succeed())
	for _, ns := range namespaces {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: user, Namespace: ns}, Rules: role.Rules})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: user, Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: user},
			Subjects:   subjects,
		})).To(Succeed())
	}

	authenticated, err := testEnv.AddUser(envtest.User{Name: user}, nil)
	Expect(err).NotTo(HaveOccurred())
	ovenConfig = authenticated.Config()
	// Same client-side rate limit as the admin config. The default (5 per second) makes the controller lag behind the
	// specs' timing; it says nothing about the rules.
	ovenConfig.QPS, ovenConfig.Burst = cfg.QPS, cfg.Burst
	return ovenConfig
}

var _ = Describe("Watched namespaces", func() {
	ctx := context.Background()

	It("handles a ServedModel in a watched namespace", func() {
		sm := newServedModel(nextWatchedNamespace(), "inside", false)
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: pvcName(sm)}, &corev1.PersistentVolumeClaim{})
		}, testTimeout, testPoll).Should(Succeed())
		Eventually(phaseOf(ctx, client.ObjectKeyFromObject(sm)), testTimeout, testPoll).ShouldNot(BeEmpty())
	})

	It("leaves a ServedModel in any other namespace alone", func() {
		outside := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "outside-"}}
		Expect(k8sClient.Create(ctx, outside)).To(Succeed())
		sm := newServedModel(outside.Name, "outside", false)
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		Consistently(func(g Gomega) {
			got := &servingv1alpha1.ServedModel{}
			g.Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			g.Expect(got.Status.Phase).To(BeEmpty())
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: pvcName(sm)}, &corev1.PersistentVolumeClaim{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "PVC: %v", err)
		}, 3*time.Second, testPoll).Should(Succeed())
	})

	It("gives the controller no write outside the watched namespaces", func() {
		// The same rules, checked directly: even a request the cache would never make is refused.
		outside := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "outside-"}}
		Expect(k8sClient.Create(ctx, outside)).To(Succeed())
		oven, err := client.New(ovenConfig, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		err = oven.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: outside.Name}})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "create outside: %v", err)
		err = oven.List(ctx, &corev1.PodList{}, client.InNamespace(outside.Name))
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "list pods outside: %v", err)
		err = oven.List(ctx, &corev1.NodeList{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "list nodes: %v", err)
	})
})
