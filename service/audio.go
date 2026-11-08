package service

import (
	"fmt"
	"strings"

	"github.com/MoeForge/moego/base64"
)

func parseAudio(audioBase64 string, format string) (duration float64, err error) {
	audioData, err := base64.StdEncoding.DecodeString(audioBase64)
	if err != nil {
		return 0, fmt.Errorf("base64 decode error: %v", err)
	}

	return realtimeAudioDuration(int64(len(audioData)), format), nil
}

// NormalizeRealtimeAudioFormat maps the native GA and preview codec names to
// the formats used by the shared byte-duration counter.
func NormalizeRealtimeAudioFormat(format string) (string, bool) {
	switch format {
	case "pcm16", "audio/pcm":
		return "pcm16", true
	case "g711_ulaw", "audio/pcmu":
		return "g711_ulaw", true
	case "g711_alaw", "audio/pcma":
		return "g711_alaw", true
	default:
		return "", false
	}
}

func realtimeAudioDuration(byteCount int64, format string) float64 {
	if format == "pcm16" {
		return float64(byteCount/2) / 24000
	}
	return float64(byteCount) / 8000
}

func DecodeBase64AudioData(audioBase64 string) (string, error) {
	// 检查并移除 data:audio/xxx;base64, 前缀
	idx := strings.Index(audioBase64, ",")
	if idx != -1 {
		audioBase64 = audioBase64[idx+1:]
	}

	// 校验 Base64 数据
	_, err := base64.StdEncoding.ValidateString(audioBase64)
	if err != nil {
		return "", fmt.Errorf("base64 decode error: %v", err)
	}

	return audioBase64, nil
}
