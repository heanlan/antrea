// Copyright 2022 Antrea Authors
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

package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"antrea.io/antrea/pkg/antctl/raw"
	"antrea.io/antrea/pkg/antctl/raw/multicluster/common"
)

const (
	leaderRole = "leader"
	memberRole = "member"

	latestVersionURL     = "https://raw.githubusercontent.com/antrea-io/antrea/main/multicluster/build/yamls"
	downloadURL          = "https://github.com/antrea-io/antrea/releases/download"
	leaderGlobalYAML     = "antrea-multicluster-leader-global.yml"
	leaderNamespacedYAML = "antrea-multicluster-leader-namespaced.yml"
	memberYAML           = "antrea-multicluster-member.yml"
)

var httpGet = http.Get

func generateManifests(role string, version string) ([]string, error) {
	var manifests []string
	switch role {
	case leaderRole:
		manifests = []string{
			fmt.Sprintf("%s/%s", latestVersionURL, leaderGlobalYAML),
			fmt.Sprintf("%s/%s", latestVersionURL, leaderNamespacedYAML),
		}
		if version != "latest" {
			manifests = []string{
				fmt.Sprintf("%s/%s/%s", downloadURL, version, leaderGlobalYAML),
				fmt.Sprintf("%s/%s/%s", downloadURL, version, leaderNamespacedYAML),
			}
		}
	case memberRole:
		manifests = []string{
			fmt.Sprintf("%s/%s", latestVersionURL, memberYAML),
		}
		if version != "latest" {
			manifests = []string{
				fmt.Sprintf("%s/%s/%s", downloadURL, version, memberYAML),
			}
		}
	default:
		return nil, fmt.Errorf("invalid role: %s", role)
	}
	return manifests, nil
}

func createResources(cmd *cobra.Command, k8sClient kubernetes.Interface, dynamicClient dynamic.Interface, content []byte) error {
	var err error
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(content)), 100)
	for {
		var rawObj runtime.RawExtension
		if err = decoder.Decode(&rawObj); err != nil {
			break
		}

		obj, gvk, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(rawObj.Raw, nil, nil)
		if err != nil {
			return err
		}
		unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return err
		}

		unstructuredObj := &unstructured.Unstructured{Object: unstructuredMap}

		gr, err := restmapper.GetAPIGroupResources(k8sClient.Discovery())
		if err != nil {
			return err
		}

		mapper := restmapper.NewDiscoveryRESTMapper(gr)
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return err
		}

		var dri dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			dri = dynamicClient.Resource(mapping.Resource).Namespace(unstructuredObj.GetNamespace())
		} else {
			dri = dynamicClient.Resource(mapping.Resource)
		}

		if _, err := dri.Create(context.TODO(), unstructuredObj, metav1.CreateOptions{}); err != nil {
			if !kerrors.IsAlreadyExists(err) {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s/%s already exists\n", unstructuredObj.GetKind(), unstructuredObj.GetName())
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%s/%s created\n", unstructuredObj.GetKind(), unstructuredObj.GetName())
		}
	}
	return nil
}

func deploy(cmd *cobra.Command, role string, version string, namespace string, filename string) error {
	kubeconfig, err := raw.ResolveKubeconfig(cmd)
	if err != nil {
		return err
	}
	restconfigTmpl := rest.CopyConfig(kubeconfig)
	raw.SetupKubeconfig(restconfigTmpl)

	k8sClient, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(kubeconfig)
	if err != nil {
		return err
	}

	if filename != "" {
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		if err := createResources(cmd, k8sClient, dynamicClient, content); err != nil {
			return err
		}
	} else {
		manifests, err := generateManifests(role, version)
		if err != nil {
			return err
		}
		for _, manifest := range manifests {
			// #nosec G107
			resp, err := httpGet(manifest)
			if err != nil {
				return err
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			content := string(b)
			if role == leaderRole && strings.Contains(manifest, "namespaced") && namespace != common.DefaultLeaderNamespace {
				content = strings.ReplaceAll(content, common.DefaultLeaderNamespace, namespace)
			}
			if role == memberRole && strings.Contains(manifest, "member") && namespace != common.DefaultMemberNamespace {
				content = strings.ReplaceAll(content, common.DefaultMemberNamespace, namespace)
			}
			if err := createResources(cmd, k8sClient, dynamicClient, []byte(content)); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Antrea Multi-cluster successfully deployed\n")
	return nil
}
