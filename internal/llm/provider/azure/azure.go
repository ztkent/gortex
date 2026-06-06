// Package azure is the Azure OpenAI Service llm.Provider.
//
// Azure speaks the same Chat Completions wire format as api.openai.com,
// so it rides the shared openaicompat.Client — but it addresses and
// authenticates requests differently, and a bare RemoteConfig.BaseURL
// override cannot express that:
//
//   - the model is selected by a *deployment name* folded into the URL
//     path, not a `model` field;
//   - the request is pinned to an API contract by an `api-version`
//     query parameter;
//   - the key travels in an `api-key` header, not a Bearer token.
//
// New builds the deployment URL + api-key header once and hands the
// rest to openaicompat.Client. It is pure Go — available in every build.
package azure

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/provider/openaicompat"
)

// New constructs the Azure OpenAI provider. The endpoint may be set in
// config or read from the env var named by EndpointEnv (default
// AZURE_OPENAI_ENDPOINT); the key is read from APIKeyEnv (default
// AZURE_OPENAI_API_KEY). A missing deployment, endpoint, or key is a
// hard error so misconfiguration surfaces at startup, not on the first
// query.
func New(cfg llm.AzureConfig) (llm.Provider, error) {
	deployment := strings.TrimSpace(cfg.Deployment)
	if deployment == "" {
		return nil, errors.New("azure: llm.azure.deployment is empty")
	}

	endpointEnv := strings.TrimSpace(cfg.EndpointEnv)
	if endpointEnv == "" {
		endpointEnv = "AZURE_OPENAI_ENDPOINT"
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv(endpointEnv))
	}
	if endpoint == "" {
		return nil, fmt.Errorf("azure: endpoint not set (config llm.azure.endpoint or env %q)", endpointEnv)
	}
	endpoint = strings.TrimRight(endpoint, "/")

	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "AZURE_OPENAI_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		return nil, fmt.Errorf("azure: API key env %q is not set", keyEnv)
	}

	apiVersion := strings.TrimSpace(cfg.APIVersion)
	if apiVersion == "" {
		apiVersion = "2024-10-21"
	}

	// Azure routes by deployment; the request-body model field is
	// mostly cosmetic but some gateways echo it, so default it to the
	// deployment name when the user hasn't pinned a logical model.
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = deployment
	}

	reqURL := fmt.Sprintf(
		"%s/openai/deployments/%s/chat/completions?api-version=%s",
		endpoint,
		url.PathEscape(deployment),
		url.QueryEscape(apiVersion),
	)

	return &openaicompat.Client{
		ProviderID: "azure",
		Tag:        "azure",
		Model:      model,
		URL:        reqURL,
		Headers:    map[string]string{"api-key": key},
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
		SchemaMode: openaicompat.SchemaJSONSchema,
	}, nil
}
