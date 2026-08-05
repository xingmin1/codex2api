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
	// Both backends resolve the same target box first. The requested size is
	// the authoritative target: asking for 2048x2048 must not produce the 2K
	// tier's 2560 long side just because that is how the tier is named.
	width, height := imageDimensions(imageBytes)
	targetLongSide := imageproc.UpscaleLongSide(scale)
	if width <= 0 || height <= 0 || targetLongSide <= 0 {
		return nil, "", "", fmt.Errorf("image upscaler: invalid source or target dimensions")
	}
	targetWidth, targetHeight, exactTarget := imageUpscaleTargetDimensions(width, height, targetLongSide, requestedSize)

	endpoint := strings.TrimSpace(os.Getenv("IMAGE_UPSCALER_ENDPOINT"))
	if endpoint == "" {
		data, contentType, err := imageproc.DoUpscaleTo(imageBytes, targetWidth, targetHeight, exactTarget)
		return data, contentType, "catmull-rom", err
	}

	if width >= targetWidth && height >= targetHeight {
		return nil, "", "realesrgan", nil
	}

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
	// Fit inside the target box by default so a source whose aspect ratio does
	// not match the requested size is scaled down rather than cropped, matching
	// the local backend. When the ratios do match, inside and cover produce the
	// same result, so an exact requested size is still hit exactly. Operators
	// who would rather fill the box can opt into cropping with
	// IMAGE_UPSCALER_FIT=cover.
	query.Set("fit", normalizeImageUpscalerFit(os.Getenv("IMAGE_UPSCALER_FIT")))
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
	contentType := normalizeImageUpscalerContentType(response.Header.Get("Content-Type"))
	if contentType == "" {
		return nil, "", "", fmt.Errorf("image upscaler returned unsupported content type %q", response.Header.Get("Content-Type"))
	}
	return body, contentType, method, nil
}

// normalizeImageUpscalerContentType keeps the stored asset MIME type within
// image/*. The value ends up on the asset record and is echoed back inline by
// the asset route, so a compromised or man-in-the-middled upscaler must not be
// able to make the gateway serve markup from its own origin. An empty or
// generic binary type is treated as the PNG the request asked for; anything
// else outside image/* is rejected.
func normalizeImageUpscalerContentType(value string) string {
	mediaType := strings.ToLower(strings.TrimSpace(value))
	if index := strings.Index(mediaType, ";"); index >= 0 {
		mediaType = strings.TrimSpace(mediaType[:index])
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		return "image/png"
	}
	if strings.HasPrefix(mediaType, "image/") {
		return mediaType
	}
	return ""
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
