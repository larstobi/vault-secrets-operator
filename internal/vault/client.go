// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	secretsv1alpha1 "github.com/hashicorp/vault-secrets-operator/api/v1alpha1"
	"github.com/hashicorp/vault-secrets-operator/internal/common"
)

var mu sync.Mutex

func NewClient(ctx context.Context, client ctrlclient.Client, obj ctrlclient.Object) (Client, error) {
	// TODO add logic for restoration from cache
	c := &defaultClient{}
	if err := c.Init(ctx, client, obj); err != nil {
		return nil, err
	}
	return c, nil
}

type Client interface {
	Init(context.Context, ctrlclient.Client, ctrlclient.Object) error
	Read(context.Context, string) (*api.Secret, error)
	Restore(context.Context, *api.Secret, types.UID) error
	Renew(context.Context) error
	Write(context.Context, string, map[string]any) (*api.Secret, error)
	GetLastResponse() (*api.Secret, error)
	CheckExpiry(int64) (bool, error)
	GetTokenTTL() (time.Duration, error)
	Login(context.Context, ctrlclient.Client) error
	GetVaultAuthObj() (*secretsv1alpha1.VaultAuth, error)
	GetVaultConnectionObj() (*secretsv1alpha1.VaultConnection, error)
	GetProviderID() (types.UID, error)
	GetCacheKey() (string, error)
	KVv1(string) (*api.KVv1, error)
	KVv2(string) (*api.KVv2, error)
}

var _ Client = (*defaultClient)(nil)

type defaultClient struct {
	initialized        bool
	client             *api.Client
	authObj            *secretsv1alpha1.VaultAuth
	connObj            *secretsv1alpha1.VaultConnection
	target             ctrlclient.ObjectKey
	lastResp           *api.Secret
	lastRenewal        int64
	providerUID        types.UID
	credentialProvider CredentialProvider
	once               sync.Once
	mu                 sync.RWMutex
}

func (c *defaultClient) GetProviderID() (types.UID, error) {
	if err := c.checkInitialized(); err != nil {
		return "", err
	}

	return c.providerUID, nil
}

func (c *defaultClient) KVv1(mount string) (*api.KVv1, error) {
	if err := c.checkInitialized(); err != nil {
		return nil, err
	}

	return c.client.KVv1(mount), nil
}

func (c *defaultClient) KVv2(mount string) (*api.KVv2, error) {
	if err := c.checkInitialized(); err != nil {
		return nil, err
	}

	return c.client.KVv2(mount), nil
}

func (c *defaultClient) GetCacheKey() (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.checkInitialized(); err != nil {
		return "", err
	}

	return GenCacheClientKeyFromObjs(c.authObj, c.connObj, c.providerUID)
}

// Restore self from the provided api.Secret (should have an Auth configured).
// The provided Client Token will be renewed as well.
func (c *defaultClient) Restore(ctx context.Context, secret *api.Secret, providerUID types.UID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized {
		return fmt.Errorf("not initialized")
	}

	if secret.Auth == nil {
		return fmt.Errorf("not an auth secret")
	}

	c.lastResp = secret
	c.providerUID = providerUID
	c.client.SetToken(secret.Auth.ClientToken)

	return c.renew(ctx)
}

func (c *defaultClient) Init(ctx context.Context, client ctrlclient.Client, obj ctrlclient.Object) error {
	if c.initialized {
		return fmt.Errorf("aleady initialized")
	}

	var err error
	c.once.Do(func() {
		err = c.init(ctx, client, obj)
		if err == nil {
			c.initialized = true
		}
	})

	return err
}

func (c *defaultClient) GetTokenTTL() (time.Duration, error) {
	last, err := c.GetLastResponse()
	if err != nil {
		return 0, err
	}
	return last.TokenTTL()
}

func (c *defaultClient) CheckExpiry(offset int64) (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.initialized {
		return false, fmt.Errorf("not initialized")
	}

	if c.lastResp == nil {
		return false, fmt.Errorf("cannot check client token expiry, never logged in")
	}

	ttl := int64(c.lastResp.LeaseDuration)
	horizon := (c.lastRenewal + ttl) - offset
	if horizon <= 0 {
		return true, nil
	}

	return time.Now().After(time.Unix(horizon, 0)), nil
}

func (c *defaultClient) GetLastResponse() (*api.Secret, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.initialized {
		return nil, fmt.Errorf("not initialized")
	}

	return c.lastResp, nil
}

func (c *defaultClient) Renew(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkInitialized(); err != nil {
		return err
	}

	return c.renew(ctx)
}

func (c *defaultClient) Login(ctx context.Context, client ctrlclient.Client) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkInitialized(); err != nil {
		return err
	}

	creds, err := c.credentialProvider.GetCreds(ctx, client)
	if err != nil {
		return err
	}

	if len(c.authObj.Spec.Headers) > 0 {
		defer c.client.SetHeaders(c.client.Headers())
		headers := c.client.Headers()
		for k, v := range c.authObj.Spec.Headers {
			headers[k] = []string{v}
		}
		c.client.SetHeaders(headers)
	}

	path := fmt.Sprintf("auth/%s/login", c.authObj.Spec.Mount)
	resp, err := c.Write(ctx, path, creds)
	if err != nil {
		return err
	}

	c.client.SetToken(resp.Auth.ClientToken)

	c.lastResp = resp

	return nil
}

func (c *defaultClient) GetVaultAuthObj() (*secretsv1alpha1.VaultAuth, error) {
	if err := c.checkInitialized(); err != nil {
		return nil, err
	}

	return c.authObj, nil
}

func (c *defaultClient) GetVaultConnectionObj() (*secretsv1alpha1.VaultConnection, error) {
	if err := c.checkInitialized(); err != nil {
		return nil, err
	}

	return c.connObj, nil
}

func (c *defaultClient) Read(ctx context.Context, path string) (*api.Secret, error) {
	if err := c.checkInitialized(); err != nil {
		return nil, err
	}

	// TODO: add metrics
	return c.client.Logical().ReadWithContext(ctx, path)
}

func (c *defaultClient) Write(ctx context.Context, path string, m map[string]any) (*api.Secret, error) {
	if err := c.checkInitialized(); err != nil {
		return nil, err
	}

	// TODO: add metrics
	return c.client.Logical().WriteWithContext(ctx, path, m)
}

func (c *defaultClient) checkInitialized() error {
	if !c.initialized {
		return fmt.Errorf("not initialized")
	}
	return nil
}

func (c *defaultClient) renew(ctx context.Context) error {
	// should be called from a write locked method only
	if c.lastResp == nil {
		return fmt.Errorf("cannot renew client token, never logged in")
	}

	if !c.lastResp.Auth.Renewable {
		return fmt.Errorf("cannot renew client token, non-renewable")
	}

	resp, err := c.Write(ctx, "/auth/token/renew-self", nil)
	if err != nil {
		c.lastResp = nil
		c.lastRenewal = 0
		return err
	} else {
		c.lastResp = resp
		c.lastRenewal = time.Now().UTC().Unix()
	}
	return nil
}

func (c *defaultClient) init(ctx context.Context, client ctrlclient.Client, obj ctrlclient.Object) error {
	auth, target, err := common.GetVaultAuthAndTarget(ctx, client, obj)
	if err != nil {
		return err
	}

	connName, err := common.GetConnectionNamespacedName(auth)
	if err != nil {
		return err
	}

	conn, err := common.GetVaultConnection(ctx, client, connName)
	if err != nil {
		return err
	}

	cfg := &ClientConfig{
		Address:         conn.Spec.Address,
		SkipTLSVerify:   conn.Spec.SkipTLSVerify,
		TLSServerName:   conn.Spec.TLSServerName,
		VaultNamespace:  auth.Spec.Namespace,
		CACertSecretRef: conn.Spec.CACertSecretRef,
		K8sNamespace:    target.Namespace,
	}

	vc, err := MakeVaultClient(ctx, cfg, client)
	if err != nil {
		return err
	}

	credentialProvider, err := NewCredentialProvider(ctx, client, obj, auth.Spec.Method)
	if err != nil {
		return err
	}

	c.credentialProvider = credentialProvider
	c.providerUID = credentialProvider.GetUID()
	c.client = vc
	c.authObj = auth
	c.connObj = conn
	c.target = target

	return nil
}