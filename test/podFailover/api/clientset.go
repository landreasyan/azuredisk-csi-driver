/*
Copyright The Kubernetes Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package podfailure

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

type PodFailoverInterface interface {
	PodFailures(namespace string) PodFailureInterface
}

type PodFailoverClient struct {
	restClient rest.Interface
}

func NewForConfig(c *rest.Config) (*PodFailoverClient, error) {
	err := AddToScheme(scheme.Scheme)
	if err != nil {
		return nil, err
	}
	config := *c
	config.ContentConfig.GroupVersion = &schema.GroupVersion{Group: GroupName, Version: GroupVersion}
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	config.UserAgent = rest.DefaultKubernetesUserAgent()

	client, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, err
	}

	return &PodFailoverClient{restClient: client}, nil
}

func (c *PodFailoverClient) PodFailures(namespace string) PodFailureInterface {
	return &podFailureClient{
		client: c.restClient,
		ns:     namespace,
	}
}