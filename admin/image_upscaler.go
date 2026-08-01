package admin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/codex2api/internal/imageproc"
)

const maxImageUpscalerResponseBytes = 64 << 20

func upscaleImageBytes(ctx context.Context, imageBytes []byte, scale, requestedSize string) ([]byte, string, string, error) {
	endpoint := strings.TrimSpace(os.Getenv("IMAGE_UPSCALER_ENDPOINT"))
	if endpoint == "" {
		data, contentType, err := imageproc.DoUpscale(imageBytes, scale)
		return data, contentType, "catmull-rom", err
	}

	width, height := imageDimensions(imageBytes)
	targetLongSide := imageproc.UpscaleLongSide(scale)
	if width <= 0 || height <= 0 || targetLongSide <= 0 {
		return nil, "", "", fmt.Errorf("image upscaler: invalid source or target dimensions")
	}
	longSide := width
	if height > longSide {
		longSide = height
	}
	if longSide >= targetLongSide {
		return nil, "", "realesrgan", nil
	}
	targetWidth, targetHeight, exactTarget := imageUpscaleTargetDimensions(width, height, targetLongSide, requestedSize)

	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, "", "", fmt.Errorf("image upscaler: invalid endpoint %q", endpoint)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/upscale"
	query := parsed.Query()
	query.Set("target_width", strconv.Itoa(targetWidth))
	query.Set("target_height", strconv.Itoa(targetHeight))
	query.Set("format", "png")
	query.Set("trigger_ratio", "1")
	fit := normalizeImageUpscalerFit(os.Getenv("IMAGE_UPSCALER_FIT"))
	if exactTarget {
		fit = "cover"
	}
	query.Set("fit", fit)
	parsed.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(imageBytes))
	if err != nil {
		return nil, "", "", err
	}
	request.Header.Set("Content-Type", http.DetectContentType(imageBytes))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, "", "", fmt.Errorf("call image upscaler: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxImageUpscalerResponseBytes+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("read image upscaler response: %w", err)
	}
	if len(body) > maxImageUpscalerResponseBytes {
		return nil, "", "", fmt.Errorf("image upscaler response exceeds %d bytes", maxImageUpscalerResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if len(message) > 1024 {
			message = message[:1024]
		}
		return nil, "", "", fmt.Errorf("image upscaler returned %d: %s", response.StatusCode, message)
	}
	applied := true
	if marker := strings.TrimSpace(response.Header.Get("X-Upscale-Applied")); marker != "" {
		parsed, parseErr := strconv.ParseBool(marker)
		if parseErr != nil {
			return nil, "", "", fmt.Errorf("image upscaler returned an invalid applied marker %q", marker)
		}
		applied = parsed
	}
	method := strings.TrimSpace(response.Header.Get("X-Upscale-Method"))
	if method == "" {
		method = "realesrgan-general-x4v3"
	}
	if !applied {
		return nil, "", method, nil
	}
	if len(body) == 0 {
		return nil, "", "", fmt.Errorf("image upscaler returned empty image data")
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "image/png"
	}
	return body, contentType, method, nil
}

func imageUpscalerBackend() string {
	if strings.TrimSpace(os.Getenv("IMAGE_UPSCALER_ENDPOINT")) != "" {
		return "external"
	}
	return "local"
}

func imageUpscaleTargetDimensions(width, height, targetLongSide int, requestedSize string) (int, int, bool) {
	requestedSize = strings.ToLower(strings.TrimSpace(requestedSize))
	if targetLongSide == imageproc.UpscaleLongSide(imageproc.Upscale2K) {
		switch requestedSize {
		case "2048x2048":
			return 2048, 2048, true
		case "2560x1440":
			return 2560, 1440, true
		case "1440x2560":
			return 1440, 2560, true
		}
	}
	if targetLongSide == imageproc.UpscaleLongSide(imageproc.Upscale4K) {
		switch requestedSize {
		case "3840x2160":
			return 3840, 2160, true
		case "2160x3840":
			return 2160, 3840, true
		case "2880x2880":
			return 2880, 2880, true
		}
	}

	targetWidth := targetLongSide
	targetHeight := max(1, height*targetLongSide/width)
	if height > width {
		targetHeight = targetLongSide
		targetWidth = max(1, width*targetLongSide/height)
	}
	return targetWidth, targetHeight, false
}

func normalizeImageUpscalerFit(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "cover") {
		return "cover"
	}
	return "inside"
}
