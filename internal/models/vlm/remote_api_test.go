package vlm

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/models/provider"
	openai "github.com/sashabaranov/go-openai"
)

// TestRemoteAPIVLMImageDetail pins the image_url detail behavior per
// provider. detail is OpenAI-specific: OpenAI-compatible gateways reject or
// ignore it (MiniMax answers HTTP 400, error 2013, "invalid image detail:
// auto" — also reachable through generic third-party proxies), so it is only
// sent to OpenAI / Azure OpenAI and omitted for everyone else.
func TestRemoteAPIVLMImageDetail(t *testing.T) {
	cases := []struct {
		name     string
		provider provider.ProviderName
		want     openai.ImageURLDetail
	}{
		{"openai keeps auto", provider.ProviderOpenAI, openai.ImageURLDetailAuto},
		{"azure keeps auto", provider.ProviderAzureOpenAI, openai.ImageURLDetailAuto},
		{"generic omits detail", provider.ProviderGeneric, ""},
		{"minimax omits detail", provider.ProviderMiniMax, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &RemoteAPIVLM{providerName: tc.provider}
			if got := v.imageDetail(); got != tc.want {
				t.Errorf("imageDetail() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewRemoteAPIVLM_ResolvesMiniMax ensures the MiniMax provider name
// reaches the instance both from the explicit provider field and from
// baseURL detection, so the detail-omission path actually triggers for
// MiniMax models configured through the model registry.
func TestNewRemoteAPIVLM_ResolvesMiniMax(t *testing.T) {
	explicit, err := NewRemoteAPIVLM(&Config{
		BaseURL:   "https://api.minimaxi.com/v1",
		APIKey:    "k",
		ModelName: "MiniMax-M3",
		Provider:  "minimax",
	})
	if err != nil {
		t.Fatalf("NewRemoteAPIVLM(explicit): %v", err)
	}
	if explicit.providerName != provider.ProviderMiniMax {
		t.Errorf("explicit provider = %q, want minimax", explicit.providerName)
	}

	detected, err := NewRemoteAPIVLM(&Config{
		BaseURL:   "https://api.minimaxi.com/v1",
		APIKey:    "k",
		ModelName: "MiniMax-M3",
	})
	if err != nil {
		t.Fatalf("NewRemoteAPIVLM(detected): %v", err)
	}
	if detected.providerName != provider.ProviderMiniMax {
		t.Errorf("detected provider = %q, want minimax", detected.providerName)
	}
}
