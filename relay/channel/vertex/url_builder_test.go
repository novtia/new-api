package vertex

import (
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
)

func TestBuildGoogleModelURLUsesTheLocationHost(t *testing.T) {
	tests := []struct {
		region string
		want   string
	}{
		{"global", "https://aiplatform.googleapis.com/v1/projects/my-project/locations/global/publishers/google/models/gemini-4-argon:generateContent"},
		{"us-central1", "https://us-central1-aiplatform.googleapis.com/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-4-argon:generateContent"},
		{"us", "https://aiplatform.us.rep.googleapis.com/v1/projects/my-project/locations/us/publishers/google/models/gemini-4-argon:generateContent"},
		{"eu", "https://aiplatform.eu.rep.googleapis.com/v1/projects/my-project/locations/eu/publishers/google/models/gemini-4-argon:generateContent"},
	}
	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			got := BuildGoogleModelURL("", DefaultAPIVersion, "my-project", tt.region, "gemini-4-argon", "generateContent")
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSplitAPIKeyProject(t *testing.T) {
	key, project := SplitAPIKeyProject("AQ.abc")
	assert.Equal(t, "AQ.abc", key)
	assert.Equal(t, "", project)

	key, project = SplitAPIKeyProject("  AQ.abc | agent-511515 \n")
	assert.Equal(t, "AQ.abc", key)
	assert.Equal(t, "agent-511515", project)

	key, project = SplitAPIKeyProject("AQ.abc|")
	assert.Equal(t, "AQ.abc", key)
	assert.Equal(t, "", project)
}

func apiKeyInfo(apiKey, region, model string) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		OriginModelName: model,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            apiKey,
			ApiVersion:        region,
			UpstreamModelName: model,
			ChannelOtherSettings: dto.ChannelOtherSettings{
				VertexKeyType: dto.VertexKeyTypeAPIKey,
			},
		},
	}
}

func TestAPIKeyRequestURL(t *testing.T) {
	a := &Adaptor{RequestMode: RequestModeGemini}
	regions := `{"default":"us-central1","gemini-4-argon":"global"}`

	tests := []struct {
		name   string
		apiKey string
		region string
		model  string
		want   string
	}{
		{
			name:   "no project keeps the legacy URL",
			apiKey: "K",
			region: "global",
			model:  "gemini-3.8-flash",
			want:   "https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-3.8-flash:generateContent?key=K",
		},
		{
			name:   "project pins the global endpoint",
			apiKey: "K|agent-511515",
			region: regions,
			model:  "gemini-4-argon",
			want:   "https://aiplatform.googleapis.com/v1/projects/agent-511515/locations/global/publishers/google/models/gemini-4-argon:generateContent?key=K",
		},
		{
			name:   "project pins a regional endpoint via default",
			apiKey: "K|agent-511515",
			region: regions,
			model:  "gemini-3.8-flash",
			want:   "https://us-central1-aiplatform.googleapis.com/v1/projects/agent-511515/locations/us-central1/publishers/google/models/gemini-3.8-flash:generateContent?key=K",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := apiKeyInfo(tc.apiKey, tc.region, tc.model)
			got, err := a.getRequestUrl(info, tc.model, "generateContent")
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("stream suffix appends key with ampersand", func(t *testing.T) {
		info := apiKeyInfo("K|p", "global", "gemini-4-argon")
		got, err := a.getRequestUrl(info, "gemini-4-argon", "streamGenerateContent?alt=sse")
		assert.NoError(t, err)
		assert.Equal(t, "https://aiplatform.googleapis.com/v1/projects/p/locations/global/publishers/google/models/gemini-4-argon:streamGenerateContent?alt=sse&key=K", got)
	})

	t.Run("configured project id overrides the key suffix", func(t *testing.T) {
		info := apiKeyInfo("K|old", "global", "gemini-4-argon")
		info.ChannelOtherSettings.VertexProjectID = " agent-511515 "
		got, err := a.getRequestUrl(info, "gemini-4-argon", "generateContent")
		assert.NoError(t, err)
		assert.Equal(t, "https://aiplatform.googleapis.com/v1/projects/agent-511515/locations/global/publishers/google/models/gemini-4-argon:generateContent?key=K", got)
	})

	t.Run("configured project id overrides the service account project", func(t *testing.T) {
		info := apiKeyInfo(`{"project_id":"sa-project"}`, "global", "gemini-4-argon")
		info.ChannelOtherSettings.VertexKeyType = dto.VertexKeyTypeJSON
		info.ChannelOtherSettings.VertexProjectID = "agent-511515"
		got, err := a.getRequestUrl(info, "gemini-4-argon", "generateContent")
		assert.NoError(t, err)
		assert.Equal(t, "https://aiplatform.googleapis.com/v1/projects/agent-511515/locations/global/publishers/google/models/gemini-4-argon:generateContent", got)

		info.ChannelOtherSettings.VertexProjectID = ""
		got, err = a.getRequestUrl(info, "gemini-4-argon", "generateContent")
		assert.NoError(t, err)
		assert.Contains(t, got, "/projects/sa-project/")
	})

	t.Run("project id with path characters is rejected", func(t *testing.T) {
		info := apiKeyInfo("K", "global", "gemini-4-argon")
		info.ChannelOtherSettings.VertexProjectID = "a/../b"
		_, err := a.getRequestUrl(info, "gemini-4-argon", "generateContent")
		assert.Error(t, err)
	})
}
