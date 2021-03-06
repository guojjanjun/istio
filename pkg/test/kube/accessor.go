//  Copyright 2018 Istio Authors
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package kube

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	"github.com/hashicorp/go-multierror"
	istioKube "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/framework/scopes"
	"istio.io/istio/pkg/test/util/retry"
	kubeApiCore "k8s.io/api/core/v1"
	kubeApiExt "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	kubeExtClient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	kubeApiMeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kubeClient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	kubeClientCore "k8s.io/client-go/kubernetes/typed/core/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // Needed for auth
	"k8s.io/client-go/rest"
)

const (
	workDirPrefix = "istio-kube-accessor-"
)

var (
	defaultRetryTimeout = retry.Timeout(time.Minute * 3)
	defaultRetryDelay   = retry.Delay(time.Second * 10)
)

// Accessor is a helper for accessing Kubernetes programmatically. It bundles some of the high-level
// operations that is frequently used by the test framework.
type Accessor struct {
	restConfig   *rest.Config
	ctl          *kubectl
	set          *kubeClient.Clientset
	extSet       *kubeExtClient.Clientset
	baseDir      string
	workDir      string
	workDirMutex sync.Mutex
}

// NewAccessor returns a new instance of an accessor.
func NewAccessor(kubeConfig string, baseWorkDir string) (*Accessor, error) {
	restConfig, err := istioKube.BuildClientConfig(kubeConfig, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create rest config. %v", err)
	}
	restConfig.APIPath = "/api"
	restConfig.GroupVersion = &kubeApiCore.SchemeGroupVersion
	restConfig.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}

	set, err := kubeClient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	extSet, err := kubeExtClient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	return &Accessor{
		restConfig: restConfig,
		ctl:        &kubectl{kubeConfig},
		set:        set,
		extSet:     extSet,
		baseDir:    baseWorkDir,
	}, nil
}

// getWorkDir lazy-creates the working directory for the accessor.
func (a *Accessor) getWorkDir() (string, error) {
	a.workDirMutex.Lock()
	defer a.workDirMutex.Unlock()

	workDir := a.workDir
	if workDir == "" {
		var err error
		if workDir, err = ioutil.TempDir(a.baseDir, workDirPrefix); err != nil {
			return "", err
		}
		a.workDir = workDir
	}
	return workDir, nil
}

// NewPortForwarder creates a new port forwarder.
func (a *Accessor) NewPortForwarder(options *PodSelectOptions, localPort, remotePort uint16) (PortForwarder, error) {
	return newPortForwarder(a.restConfig, options, localPort, remotePort)
}

// GetPods returns pods in the given namespace, based on the selectors. If no selectors are given, then
// all pods are returned.
func (a *Accessor) GetPods(namespace string, selectors ...string) ([]kubeApiCore.Pod, error) {
	s := strings.Join(selectors, ",")
	list, err := a.set.CoreV1().Pods(namespace).List(kubeApiMeta.ListOptions{LabelSelector: s})

	if err != nil {
		return []kubeApiCore.Pod{}, err
	}

	return list.Items, nil
}

// GetPod returns the pod with the given namespace and name.
func (a *Accessor) GetPod(namespace, name string) (*kubeApiCore.Pod, error) {
	return a.set.CoreV1().
		Pods(namespace).Get(name, kubeApiMeta.GetOptions{})
}

// FindPodBySelectors returns the first matching pod, given a namespace and a set of selectors.
func (a *Accessor) FindPodBySelectors(namespace string, selectors ...string) (kubeApiCore.Pod, error) {
	list, err := a.GetPods(namespace, selectors...)
	if err != nil {
		return kubeApiCore.Pod{}, err
	}

	if len(list) == 0 {
		return kubeApiCore.Pod{}, fmt.Errorf("no matching pod found for selectors: %v", selectors)
	}

	if len(list) > 1 {
		scopes.Framework.Warnf("More than one pod found matching selectors: %v", selectors)
	}

	return list[0], nil
}

// PodFetchFunc fetches pods from the Accessor.
type PodFetchFunc func() ([]kubeApiCore.Pod, error)

// NewPodFetch creates a new PodFetchFunction that fetches all pods matching the namespace and label selectors.
func (a *Accessor) NewPodFetch(namespace string, selectors ...string) PodFetchFunc {
	return func() ([]kubeApiCore.Pod, error) {
		return a.GetPods(namespace, selectors...)
	}
}

// NewSinglePodFetch creates a new PodFetchFunction that fetches a single pod matching the given label selectors.
func (a *Accessor) NewSinglePodFetch(namespace string, selectors ...string) PodFetchFunc {
	return func() ([]kubeApiCore.Pod, error) {
		pod, err := a.FindPodBySelectors(namespace, selectors...)
		if err != nil {
			return nil, err
		}
		return []kubeApiCore.Pod{pod}, nil
	}
}

// WaitUntilPodsAreReady waits until the pod with the name/namespace is in ready state.
func (a *Accessor) WaitUntilPodsAreReady(fetchFunc PodFetchFunc, opts ...retry.Option) error {
	_, err := retry.Do(func() (interface{}, bool, error) {

		scopes.CI.Infof("Checking pods...")

		pods, err := fetchFunc()
		if err != nil {
			scopes.CI.Infof("Failed retrieving pods: %v", err)
			return nil, false, err
		}

		for i, p := range pods {
			msg := "Ready"
			if e := checkPodReady(&p); e != nil {
				msg = e.Error()
				err = multierror.Append(err, fmt.Errorf("%s/%s: %s", p.Namespace, p.Name, msg))
			}
			scopes.CI.Infof("  [%2d] %45s %15s (%v)", i, p.Name, p.Status.Phase, msg)
		}

		if err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}, newRetryOptions(opts...)...)

	return err
}

// WaitUntilDeploymentIsReady waits until the deployment with the name/namespace is in ready state.
func (a *Accessor) WaitUntilDeploymentIsReady(ns string, name string, opts ...retry.Option) error {
	_, err := retry.Do(func() (interface{}, bool, error) {

		deployment, err := a.set.ExtensionsV1beta1().Deployments(ns).Get(name, kubeApiMeta.GetOptions{})
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil, true, err
			}
		}

		ready := deployment.Status.ReadyReplicas == deployment.Status.UnavailableReplicas+deployment.Status.AvailableReplicas

		return nil, ready, nil
	}, newRetryOptions(opts...)...)

	return err
}

// WaitUntilDaemonSetIsReady waits until the deployment with the name/namespace is in ready state.
func (a *Accessor) WaitUntilDaemonSetIsReady(ns string, name string, opts ...retry.Option) error {
	_, err := retry.Do(func() (interface{}, bool, error) {

		daemonSet, err := a.set.ExtensionsV1beta1().DaemonSets(ns).Get(name, kubeApiMeta.GetOptions{})
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil, true, err
			}
		}

		ready := daemonSet.Status.NumberReady == daemonSet.Status.DesiredNumberScheduled

		return nil, ready, nil
	}, newRetryOptions(opts...)...)

	return err
}

// DeleteValidatingWebhook deletes the validating webhook with the given name.
func (a *Accessor) DeleteValidatingWebhook(name string) error {
	return a.set.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Delete(name, deleteOptionsForeground())
}

// WaitForValidatingWebhookDeletion waits for the validating webhook with the given name to be garbage collected by kubernetes.
func (a *Accessor) WaitForValidatingWebhookDeletion(name string, opts ...retry.Option) error {
	_, err := retry.Do(func() (interface{}, bool, error) {
		if a.ValidatingWebhookConfigurationExists(name) {
			return nil, false, fmt.Errorf("validating webhook not deleted: %s", name)
		}

		// It no longer exists ... success.
		return nil, true, nil
	}, newRetryOptions(opts...)...)

	return err
}

// ValidatingWebhookConfigurationExists indicates whether a mutating validating with the given name exists.
func (a *Accessor) ValidatingWebhookConfigurationExists(name string) bool {
	_, err := a.set.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Get(name, kubeApiMeta.GetOptions{})
	return err == nil
}

// GetCustomResourceDefinitions gets the CRDs
func (a *Accessor) GetCustomResourceDefinitions() ([]kubeApiExt.CustomResourceDefinition, error) {
	crd, err := a.extSet.ApiextensionsV1beta1().CustomResourceDefinitions().List(kubeApiMeta.ListOptions{})
	if err != nil {
		return nil, err
	}
	return crd.Items, nil
}

// DeleteCustomResourceDefinitions deletes the CRD with the given name.
func (a *Accessor) DeleteCustomResourceDefinitions(name string) error {
	return a.extSet.ApiextensionsV1beta1().CustomResourceDefinitions().Delete(name, deleteOptionsForeground())
}

// GetService returns the service entry with the given name/namespace.
func (a *Accessor) GetService(ns string, name string) (*kubeApiCore.Service, error) {
	return a.set.CoreV1().Services(ns).Get(name, kubeApiMeta.GetOptions{})
}

// GetSecret returns secret resource with the given namespace.
func (a *Accessor) GetSecret(ns string) kubeClientCore.SecretInterface {
	return a.set.CoreV1().Secrets(ns)
}

// CreateNamespace with the given name. Also adds an "istio-testing" annotation.
func (a *Accessor) CreateNamespace(ns string, istioTestingAnnotation string, injectionEnabled bool) error {
	scopes.Framework.Debugf("Creating namespace: %s", ns)
	n := kubeApiCore.Namespace{
		ObjectMeta: kubeApiMeta.ObjectMeta{
			Name:   ns,
			Labels: map[string]string{},
		},
	}
	if istioTestingAnnotation != "" {
		n.ObjectMeta.Labels["istio-testing"] = istioTestingAnnotation
	}
	if injectionEnabled {
		n.ObjectMeta.Labels["istio-injection"] = "enabled"
	}

	_, err := a.set.CoreV1().Namespaces().Create(&n)
	return err
}

// NamespaceExists returns true if the given namespace exists.
func (a *Accessor) NamespaceExists(ns string) bool {
	allNs, err := a.set.CoreV1().Namespaces().List(kubeApiMeta.ListOptions{})
	if err != nil {
		return false
	}
	for _, n := range allNs.Items {
		if n.Name == ns {
			return true
		}
	}
	return false
}

// DeleteNamespace with the given name
func (a *Accessor) DeleteNamespace(ns string) error {
	scopes.Framework.Debugf("Deleting namespace: %s", ns)
	return a.set.CoreV1().Namespaces().Delete(ns, deleteOptionsForeground())
}

// WaitForNamespaceDeletion waits until a namespace is deleted.
func (a *Accessor) WaitForNamespaceDeletion(ns string, opts ...retry.Option) error {
	_, err := retry.Do(func() (interface{}, bool, error) {
		_, err2 := a.set.CoreV1().Namespaces().Get(ns, kubeApiMeta.GetOptions{})
		if err2 == nil {
			return nil, false, nil
		}

		if errors.IsNotFound(err2) {
			return nil, true, nil
		}

		return nil, true, err2
	}, newRetryOptions(opts...)...)

	return err
}

// ApplyContents applies the given config contents using kubectl.
func (a *Accessor) ApplyContents(namespace string, contents string) error {
	return a.ctl.applyContents(namespace, contents)
}

// Apply the config in the given filename using kubectl.
func (a *Accessor) Apply(namespace string, filename string) error {
	docs, err := splitYaml(filename)
	if err != nil {
		return err
	}

	workDir, err := a.getWorkDir()
	if err != nil {
		return err
	}

	for _, doc := range docs {
		f, e := doc.toTempFile(workDir)
		if e != nil {
			return multierror.Append(err, e)
		}
		scopes.CI.Infof("Applying yaml file %v", f)
		if e = a.ctl.apply(namespace, f); e != nil {
			err = multierror.Append(err, e)
		}
	}
	return err
}

// DeleteContents deletes the given config contents using kubectl.
func (a *Accessor) DeleteContents(namespace string, contents string) error {
	return a.ctl.deleteContents(namespace, contents)
}

// Delete the config in the given filename using kubectl.
func (a *Accessor) Delete(namespace string, filename string) error {
	docs, err := splitYaml(filename)
	if err != nil {
		return err
	}

	workDir, err := a.getWorkDir()
	if err != nil {
		return err
	}

	for i := len(docs) - 1; i >= 0; i-- {
		f, e := docs[i].toTempFile(workDir)
		if e != nil {
			return multierror.Append(err, e)
		}
		scopes.CI.Infof("Deleting yaml file %v", f)
		if e = a.ctl.delete(namespace, f); e != nil {
			err = multierror.Append(err, e)
		}
	}

	return err
}

// Logs calls the logs command for the specified pod, with -c, if container is specified.
func (a *Accessor) Logs(namespace string, pod string, container string) (string, error) {
	return a.ctl.logs(namespace, pod, container)
}

// Exec executes the provided command on the specified pod/container.
func (a *Accessor) Exec(namespace, pod, container, command string) (string, error) {
	return a.ctl.exec(namespace, pod, container, command)
}

func checkPodReady(pod *kubeApiCore.Pod) error {
	switch pod.Status.Phase {
	case kubeApiCore.PodSucceeded:
		return nil
	case kubeApiCore.PodRunning:
		// Wait until all containers are ready.
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if !containerStatus.Ready {
				return fmt.Errorf("container not ready: '%s'", containerStatus.Name)
			}
		}
		return nil
	default:
		return fmt.Errorf("%s", pod.Status.Phase)
	}
}

func deleteOptionsForeground() *kubeApiMeta.DeleteOptions {
	propagationPolicy := kubeApiMeta.DeletePropagationForeground
	return &kubeApiMeta.DeleteOptions{
		PropagationPolicy: &propagationPolicy,
	}
}

type yamlDoc struct {
	filePattern string
	content     string
}

func (d *yamlDoc) prepend(c string) {
	d.content = test.JoinConfigs(c, d.content)
}

func (d *yamlDoc) append(c string) {
	d.content = test.JoinConfigs(d.content, c)
}

func (d *yamlDoc) toTempFile(workDir string) (string, error) {
	f, err := ioutil.TempFile(workDir, d.filePattern)
	if err != nil {
		return "", err
	}
	defer f.Close()

	name := f.Name()

	_, err = f.WriteString(d.content)
	if err != nil {
		return "", err
	}
	return name, nil
}

// split the given yaml file into 2 docs: namespaces first, then crds, then everything else.
func splitYaml(filename string) ([]*yamlDoc, error) {
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	cfgs := test.SplitConfigs(string(content))

	_, base := filepath.Split(filename)
	ext := filepath.Ext(filename)
	nameWithoutExt := strings.TrimSuffix(base, ext)
	namespacesAndCrds := &yamlDoc{
		filePattern: fmt.Sprintf("%s_namespaces_and_crds_*%s", nameWithoutExt, ext),
	}
	misc := &yamlDoc{
		filePattern: fmt.Sprintf("%s_misc_*%s", nameWithoutExt, ext),
	}
	for _, cfg := range cfgs {
		var typeMeta kubeApiMeta.TypeMeta
		if e := yaml.Unmarshal([]byte(cfg), &typeMeta); e != nil {
			// Ignore invalid parts. This most commonly happens when it's empty or contains only comments.
			continue
		}

		switch typeMeta.Kind {
		case "Namespace":
			namespacesAndCrds.append(cfg)
		case "CustomResourceDefinition":
			namespacesAndCrds.append(cfg)
		default:
			misc.append(cfg)
		}
	}

	if err != nil {
		return nil, err
	}

	return []*yamlDoc{namespacesAndCrds, misc}, nil
}

func newRetryOptions(opts ...retry.Option) []retry.Option {
	out := make([]retry.Option, 0, 2+len(opts))
	out = append(out, defaultRetryTimeout, defaultRetryDelay)
	out = append(out, opts...)
	return out
}
