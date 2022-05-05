/*
 Copyright 2022 Michael Amann

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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"
	cmmetav1 "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"

	"github.com/gapodo/cert-manager-webhook-cloudns-v2/cloudns"
)

const ProviderName = "cloudns-v2"

var GroupName = os.Getenv("GROUP_NAME")

func main() {
	if GroupName == "" {
		GroupName = "acme.kle.li"
	}

	cmd.RunWebhookServer(GroupName,
		&clouDNSProviderSolver{},
	)
}

type clouDNSProviderSolver struct {
	client *kubernetes.Clientset
}

type clouDNSProviderConfig struct {
	AuthId      cmmetav1.SecretKeySelector `json:"authIdTokenSecretRef"`
	AuthPass    cmmetav1.SecretKeySelector `json:"authPassKeySecretRef"`
	AuthIdType  string                     `json:"authIdType"`
	TTL         int                        `json:"ttl"`
	HTTPTimeout int                        `json:"httpTimeout"`
}

func (c clouDNSProviderSolver) Name() string {
	return ProviderName
}

func (c clouDNSProviderSolver) Present(ch *v1alpha1.ChallengeRequest) error {

	config, err := newConfig(c, ch)
	if err != nil {
		return err
	}

	provider, err := cloudns.NewDNSProviderConfig(config)

	if err != nil {
		return err
	}

	return provider.Present(ch.ResolvedFQDN, ch.Key)
}

func newConfig(c clouDNSProviderSolver, ch *v1alpha1.ChallengeRequest) (*cloudns.Config, error) {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return nil, err
	}

	fmt.Printf("ClouDNS Decoded configuration: %v\n", cfg)

	authId, err := c.loadSecretData(cfg.AuthId, ch.ResourceNamespace)
	if err != nil {
		return nil, err
	}
	authPass, err := c.loadSecretData(cfg.AuthPass, ch.ResourceNamespace)
	if err != nil {
		return nil, err
	}

	// default to auth-id
	if cfg.AuthIdType == "" {
		cfg.AuthIdType = "auth-id"
		fmt.Printf("ClouDNS No value entered for \"authIdType\". Defaulting to auth-id.\n")
	} else if cfg.AuthIdType == "sub-auth-id" || cfg.AuthIdType == "auth-id" {
		// noop, already valid
		fmt.Printf("ClouDNS auth-id type: %v\n", cfg.AuthIdType)
	} else {
		return nil, fmt.Errorf("ClouDNS auth id type is not valid. Expected one of 'auth-id' or 'sub-auth-id' but was: '%s'", cfg.AuthIdType)
	}

	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = int(30 * time.Second)
	}

	var config = &cloudns.Config{
		AuthID:       string(authId),
		AuthIDType:   cfg.AuthIdType,
		AuthPassword: string(authPass),
		TTL:          ttlRounder(cfg.TTL),
		HTTPClient: &http.Client{
			Timeout: time.Duration(cfg.HTTPTimeout),
		},
	}

	return config, nil
}

// Delete TXT DNS record for DNS01
func (c clouDNSProviderSolver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	config, err := newConfig(c, ch)
	if err != nil {
		return err
	}

	provider, err := cloudns.NewDNSProviderConfig(config)

	if err != nil {
		return err
	}

	// Remove TXT DNS record
	return provider.CleanUp(ch.ResolvedFQDN, ch.Key)
}

func (c *clouDNSProviderSolver) Initialize(kubeClientConfig *rest.Config, stopCh <-chan struct{}) error {
	cl, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return err
	}

	c.client = cl

	return nil
}

// loadConfig is a small helper function that decodes JSON configuration into
// the typed config struct.
func loadConfig(cfgJSON *extapi.JSON) (clouDNSProviderConfig, error) {
	cfg := clouDNSProviderConfig{}
	// handle the 'base case' where no configuration has been provided
	if cfgJSON == nil {
		return cfg, fmt.Errorf("ClouDNS configuration has not been provided")
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("ClouDNS error decoding solver config: %v", err)
	}

	return cfg, nil
}

func (c *clouDNSProviderSolver) loadSecretData(selector cmmetav1.SecretKeySelector, ns string) ([]byte, error) {
	secret, err := c.client.CoreV1().Secrets(ns).Get(context.TODO(), selector.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("ClouDNS failed to load secret %q \n%w", ns+"/"+selector.Name, err)
	}

	if data, ok := secret.Data[selector.Key]; ok {
		return data, nil
	}

	return nil, fmt.Errorf("ClouDNS no key %q in secret %q", selector.Key, ns+"/"+selector.Name)
}

// https://www.cloudns.net/wiki/article/58/
// Available TTL's:
// 60 = 1 minute
// 300 = 5 minutes
// 900 = 15 minutes
// 1800 = 30 minutes
// 3600 = 1 hour
// 21600 = 6 hours
// 43200 = 12 hours
// 86400 = 1 day
// 172800 = 2 days
// 259200 = 3 days
// 604800 = 1 week
// 1209600 = 2 weeks
// 2592000 = 1 month
func ttlRounder(ttl int) int {
	for _, validTTL := range []int{60, 300, 900, 1800, 3600, 21600, 43200, 86400, 172800, 259200, 604800, 1209600} {
		if ttl <= validTTL {
			return validTTL
		}
	}

	return 2592000
}
