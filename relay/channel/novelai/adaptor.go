// Package novelai relays NovelAI's native image API (/ai/generate-image,
// /ai/generate-image-stream, and /ai/upscale). Requests and responses are
// forwarded as NovelAI sends them; the adaptor only authenticates upstream,
// maps the model name and counts the images in the response so billing and pool
// limits match what was actually generated.
package novelai

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

// maxResponseBytes bounds a buffered (non-stream) response. A generation
// returns a zip of PNGs, a few MB per image.
const maxResponseBytes = 256 << 20

var ModelList = []string{
	"nai-diffusion-5-full",
	"nai-diffusion-5-full-medium",
	"nai-diffusion-5-curated",
	"nai-diffusion-5-full-inpainting",
	"nai-diffusion-5-curated-inpainting",
	"nai-diffusion-4-5-full",
	"nai-diffusion-4-5-curated",
	"nai-diffusion-4-full",
	"nai-diffusion-4-curated-preview",
	"nai-diffusion-3",
	"nai-diffusion-furry-3",
}

type Adaptor struct{}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	baseURL := info.ChannelBaseUrl
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeNovelAI)
	}
	// The path is the client's own NovelAI path, without any query string.
	path, _, _ := strings.Cut(info.RequestURLPath, "?")
	return strings.TrimRight(baseURL, "/") + path, nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	if info.ApiKey == "" {
		return errors.New("novelai adaptor: api key is required")
	}
	req.Set("Authorization", "Bearer "+info.ApiKey)
	req.Set("Content-Type", "application/json")
	path, _, _ := strings.Cut(info.RequestURLPath, "?")
	switch {
	case info.IsStream:
		req.Set("Accept", "text/event-stream")
	case strings.HasSuffix(path, "/upscale"):
		req.Set("Accept", "*/*")
	default:
		req.Set("Accept", "application/x-zip-compressed")
	}
	return nil
}

// ConvertImageRequest forwards the client's NovelAI body untouched apart from
// the mapped model name. A top-level "n" is removed because it is not a
// NovelAI field and would otherwise be read as the billing quantity.
func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	body, err := storage.Bytes()
	if err != nil {
		return nil, err
	}
	if body, err = sjson.SetBytes(body, "model", request.Model); err != nil {
		return nil, err
	}
	if body, err = sjson.DeleteBytes(body, "n"); err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if resp == nil {
		return nil, types.NewError(errors.New("novelai adaptor: empty response"), types.ErrorCodeBadResponse)
	}
	defer service.CloseResponseBodyGracefully(resp)

	if info.IsStream {
		return &dto.Usage{}, streamResponse(c, resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	if len(body) > maxResponseBytes {
		return nil, types.NewError(fmt.Errorf("novelai response exceeds %d bytes", maxResponseBytes), types.ErrorCodeBadResponseBody)
	}
	info.UpdateImageCount(int64(countPNGs(body)))
	service.IOCopyBytesGracefully(c, resp, body)
	return &dto.Usage{}, nil
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// countPNGs counts the images in a generation or upscale response: either a raw
// PNG payload or a zip with one PNG per sample. An unreadable archive counts as
// zero, which leaves the requested quantity (bounded at request validation) in
// place.
func countPNGs(body []byte) int {
	if bytes.HasPrefix(body, pngSignature) {
		return 1
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return 0
	}
	count := 0
	for _, file := range archive.File {
		if strings.HasSuffix(strings.ToLower(file.Name), ".png") {
			count++
		}
	}
	return count
}

// streamResponse relays NovelAI's event stream as it arrives. The stream
// endpoint is limited to one sample at request validation, so the requested
// quantity is already the billable one.
func streamResponse(c *gin.Context, resp *http.Response) *types.NewAPIError {
	for k, v := range resp.Header {
		if service.ShouldCopyUpstreamHeader(c, k, v) {
			c.Writer.Header().Set(k, v[0])
		}
	}
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(resp.StatusCode)

	buffer := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buffer[:n]); writeErr != nil {
				return nil
			}
			c.Writer.Flush()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return types.NewError(err, types.ErrorCodeBadResponse)
		}
	}
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return "novelai"
}

func (a *Adaptor) ConvertOpenAIRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeneralOpenAIRequest) (any, error) {
	return nil, errors.New("novelai adaptor: only the native image API is supported")
}

func (a *Adaptor) ConvertRerankRequest(*gin.Context, int, dto.RerankRequest) (any, error) {
	return nil, errors.New("novelai adaptor: only the native image API is supported")
}

func (a *Adaptor) ConvertEmbeddingRequest(*gin.Context, *relaycommon.RelayInfo, dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("novelai adaptor: only the native image API is supported")
}

func (a *Adaptor) ConvertAudioRequest(*gin.Context, *relaycommon.RelayInfo, dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("novelai adaptor: only the native image API is supported")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(*gin.Context, *relaycommon.RelayInfo, dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("novelai adaptor: only the native image API is supported")
}

func (a *Adaptor) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("novelai adaptor: only the native image API is supported")
}

func (a *Adaptor) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("novelai adaptor: only the native image API is supported")
}
