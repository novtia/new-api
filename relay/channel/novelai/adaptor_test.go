package novelai

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newContext(t *testing.T, path, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func zipWith(t *testing.T, names ...string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range names {
		file, err := writer.Create(name)
		require.NoError(t, err)
		_, err = file.Write([]byte("data"))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func TestNovelAIRequestIsHeldToTheFreeTier(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		parameters string
		extra      string
		wantErr    string
	}{
		{name: "free-tier request passes", path: "/ai/generate-image", parameters: `{"width":832,"height":1216,"steps":28,"n_samples":1}`},
		{name: "square limit is inclusive", path: "/ai/generate-image", parameters: `{"width":1024,"height":1024}`},
		{name: "oversized area is rejected", path: "/ai/generate-image", parameters: `{"width":1472,"height":1472}`, wantErr: "pixels"},
		{name: "overflowing dimensions are rejected", path: "/ai/generate-image", parameters: `{"width":4294967296,"height":4294967296}`, wantErr: "pixels"},
		{name: "missing size is rejected", path: "/ai/generate-image", parameters: `{"steps":28}`, wantErr: "width"},
		{name: "too many steps are rejected", path: "/ai/generate-image", parameters: `{"width":512,"height":512,"steps":50}`, wantErr: "steps"},
		{name: "extra samples are rejected", path: "/ai/generate-image", parameters: `{"width":512,"height":512,"n_samples":4}`, wantErr: "n_samples"},
		{name: "zero samples are rejected", path: "/ai/generate-image", parameters: `{"width":512,"height":512,"n_samples":0}`, wantErr: "n_samples"},
		{name: "fractional samples are rejected", path: "/ai/generate-image", parameters: `{"width":512,"height":512,"n_samples":1.5}`, wantErr: "n_samples"},
		{name: "unlisted action is rejected", path: "/ai/generate-image", parameters: `{"width":512,"height":512}`, extra: `"action":"upscale",`, wantErr: "action"},
		{name: "stream endpoint is a stream", path: "/ai/generate-image-stream", parameters: `{"width":512,"height":512}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"input":"1girl","model":"nai-diffusion-4-5-full",` + tc.extra + `"parameters":` + tc.parameters + `}`
			request, err := helper.GetAndValidateNovelAIRequest(newContext(t, tc.path, body))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "nai-diffusion-4-5-full", request.Model)
			assert.Equal(t, "1girl", request.Prompt)
			assert.Equal(t, strings.HasSuffix(tc.path, "-stream"), request.Stream != nil && *request.Stream)
			count, err := request.ImageCount(false)
			require.NoError(t, err)
			assert.Equal(t, 1, count)
		})
	}
}

func TestNovelAIUpstreamBodyKeepsClientFieldsAndMappedModel(t *testing.T) {
	body := `{"input":"1girl","model":"alias","n":99,"action":"generate","parameters":{"width":512,"height":512,"seed":7}}`
	c := newContext(t, "/ai/generate-image", body)

	converted, err := (&Adaptor{}).ConvertImageRequest(c, &common.RelayInfo{}, dto.ImageRequest{Model: "nai-diffusion-4-5-full"})
	require.NoError(t, err)

	assert.JSONEq(t,
		`{"input":"1girl","model":"nai-diffusion-4-5-full","action":"generate","parameters":{"width":512,"height":512,"seed":7}}`,
		string(converted.(json.RawMessage)),
		"model is remapped, the stray top-level n is dropped, everything else is untouched")
}

func TestNovelAIBilledImageCountFollowsTheZipNotTheArchiveSize(t *testing.T) {
	assert.Equal(t, 2, countPNGs(zipWith(t, "image_0.png", "image_1.PNG", "readme.txt")))
	assert.Equal(t, 0, countPNGs(zipWith(t, "notes.txt")))
	assert.Equal(t, 0, countPNGs([]byte(`{"statusCode":500,"message":"not a zip"}`)))
	assert.Equal(t, 1, countPNGs(append(append([]byte{}, pngSignature...), []byte("ihdr")...)))
}

func TestNovelAIUpscaleValidationAndHeaders(t *testing.T) {
	cases := []struct {
		name    string
		allow   string
		body    string
		wantErr string
	}{
		{
			name: "valid upscale request passes with N=1",
			body: `{"image":"aGVsbG8=","model":"nai-diffusion-5-curated"}`,
		},
		{
			name:    "missing image is rejected",
			body:    `{"model":"nai-diffusion-5-curated"}`,
			wantErr: "image",
		},
		{
			name:    "blank image is rejected",
			body:    `{"image":"   ","model":"nai-diffusion-5-curated"}`,
			wantErr: "image",
		},
		{
			name:    "missing model is rejected",
			body:    `{"image":"aGVsbG8="}`,
			wantErr: "model",
		},
		{
			name:    "upscale disabled by env is rejected",
			allow:   "false",
			body:    `{"image":"aGVsbG8=","model":"nai-diffusion-5-curated"}`,
			wantErr: "upscale",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.allow != "" {
				t.Setenv("NOVELAI_ALLOW_UPSCALE", tc.allow)
			}
			req, err := helper.GetAndValidateNovelAIRequest(newContext(t, "/ai/upscale", tc.body))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "nai-diffusion-5-curated", req.Model)
			assert.Equal(t, "upscale", req.Size)
			count, err := req.ImageCount(false)
			require.NoError(t, err)
			assert.Equal(t, 1, count)
		})
	}

	adaptor := &Adaptor{}
	c := newContext(t, "/ai/upscale", `{}`)
	hdr := make(http.Header)
	require.NoError(t, adaptor.SetupRequestHeader(c, &hdr, &common.RelayInfo{
		ChannelMeta: &common.ChannelMeta{
			ApiKey: "pst-test",
		},
		RequestURLPath: "/ai/upscale",
	}))
	assert.Equal(t, "*/*", hdr.Get("Accept"))
	assert.Equal(t, "Bearer pst-test", hdr.Get("Authorization"))

	assert.Contains(t, adaptor.GetModelList(), "nai-diffusion-5-full")
	assert.Contains(t, adaptor.GetModelList(), "nai-diffusion-5-full-medium")
	assert.Contains(t, adaptor.GetModelList(), "nai-diffusion-5-curated")
	assert.Contains(t, adaptor.GetModelList(), "nai-diffusion-5-full-inpainting")
	assert.Contains(t, adaptor.GetModelList(), "nai-diffusion-5-curated-inpainting")
}
