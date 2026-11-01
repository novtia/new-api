package helper

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// novelAIFreeTier bounds a request to what a NovelAI subscription generates
// without spending Anlas. Every request through the gateway is paid for by the
// channel owner's account, so anything beyond the free tier is rejected here
// instead of being left for the upstream to charge.
type novelAIFreeTier struct {
	maxPixels  int
	maxSteps   int
	maxSamples int
	actions    []string
}

func loadNovelAIFreeTier() novelAIFreeTier {
	tier := novelAIFreeTier{
		maxPixels:  common.GetEnvOrDefault("NOVELAI_MAX_PIXELS", 1024*1024),
		maxSteps:   common.GetEnvOrDefault("NOVELAI_MAX_STEPS", 28),
		maxSamples: min(common.GetEnvOrDefault("NOVELAI_MAX_SAMPLES", 1), dto.MaxImageN),
	}
	for action := range strings.SplitSeq(common.GetEnvOrDefaultString("NOVELAI_ALLOWED_ACTIONS", "generate,img2img,infill"), ",") {
		tier.actions = append(tier.actions, strings.TrimSpace(action))
	}
	return tier
}

// GetAndValidateNovelAIRequest parses a NovelAI native image request
// (/ai/generate-image and /ai/generate-image-stream) into the image request
// that drives routing, billing and pool accounting. The original body is what
// reaches the upstream; this view only carries the fields those need.
func GetAndValidateNovelAIRequest(c *gin.Context) (*dto.ImageRequest, error) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	body, err := storage.Bytes()
	if err != nil {
		return nil, err
	}
	if !gjson.ValidBytes(body) {
		return nil, errors.New("request body is not valid JSON")
	}

	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		return nil, errors.New("model is required")
	}
	path, _, _ := strings.Cut(c.Request.URL.Path, "?")
	if strings.HasSuffix(path, "/upscale") {
		if !common.GetEnvOrDefaultBool("NOVELAI_ALLOW_UPSCALE", true) {
			return nil, errors.New("upscale is not allowed")
		}
		image := gjson.GetBytes(body, "image")
		if image.Type != gjson.String || strings.TrimSpace(image.String()) == "" {
			return nil, errors.New("image is required")
		}
		return &dto.ImageRequest{
			Model: model,
			N:     common.GetPointer(uint(1)),
			Size:  "upscale",
		}, nil
	}
	parameters := gjson.GetBytes(body, "parameters")
	if !parameters.IsObject() {
		return nil, errors.New("parameters is required")
	}

	tier := loadNovelAIFreeTier()
	action := strings.TrimSpace(gjson.GetBytes(body, "action").String())
	if action == "" {
		action = "generate"
	}
	if !slices.Contains(tier.actions, action) {
		return nil, fmt.Errorf("action %q is not allowed", action)
	}

	width, height := parameters.Get("width"), parameters.Get("height")
	if !width.Exists() || !height.Exists() || float64(width.Int()) != width.Float() || float64(height.Int()) != height.Float() {
		return nil, errors.New("parameters.width and parameters.height must be integers")
	}
	if width.Int() <= 0 || height.Int() <= 0 || width.Int() > 1<<16 || height.Int() > 1<<16 || width.Int()*height.Int() > int64(tier.maxPixels) {
		return nil, fmt.Errorf("image area must not exceed %d pixels", tier.maxPixels)
	}

	if steps := parameters.Get("steps"); steps.Exists() && (steps.Int() < 1 || steps.Int() > int64(tier.maxSteps)) {
		return nil, fmt.Errorf("parameters.steps must be between 1 and %d", tier.maxSteps)
	}

	samples := int64(1)
	if node := parameters.Get("n_samples"); node.Exists() {
		samples = node.Int()
		if float64(samples) != node.Float() {
			return nil, errors.New("parameters.n_samples must be an integer")
		}
	}
	if samples < 1 || samples > int64(tier.maxSamples) {
		return nil, fmt.Errorf("parameters.n_samples must be between 1 and %d", tier.maxSamples)
	}

	request := &dto.ImageRequest{
		Model:  model,
		Prompt: gjson.GetBytes(body, "input").String(),
		N:      common.GetPointer(uint(samples)),
		Size:   fmt.Sprintf("%dx%d", width.Int(), height.Int()),
	}
	if strings.HasSuffix(c.Request.URL.Path, "-stream") {
		request.Stream = common.GetPointer(true)
	}
	return request, nil
}
