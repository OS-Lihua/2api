package volcengine

import (
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/MoeForge/moego/ojson"

	"github.com/QuantumNous/new-api/common"
	channelconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

type speechRequestBody struct {
	io.Reader
	query, submit bool
}

// The default V1 service supports HTTP query and WebSocket submit. Explicit
// proxy endpoints keep their selected transport.
func speechTransports(baseURL string) (query, submit bool) {
	if baseURL == "" || baseURL == channelconstant.GetChannelBaseURL(channelconstant.ChannelTypeVolcEngine) {
		return true, true
	}
	if strings.HasPrefix(baseURL, "ws://") || strings.HasPrefix(baseURL, "wss://") {
		return false, true
	}
	return true, false
}

func (p speechRequestBody) ProjectAudioRequest(body []byte, public dto.AudioRequest) (*dto.AudioRequest, error) {
	if public.HasSpeechConditioning() {
		return nil, fmt.Errorf("Volcengine speech cannot preserve the requested conditioning inputs")
	}
	for _, fields := range [][]byte{body, []byte(ojson.GetBytes(body, "request").Raw)} {
		var conditioning dto.AudioRequest
		if err := common.Unmarshal(fields, &conditioning); err != nil {
			return nil, err
		}
		if conditioning.HasSpeechConditioning() {
			return nil, fmt.Errorf("Volcengine speech cannot preserve the requested conditioning inputs")
		}
	}
	if len(public.MaxNewTokens) > 0 || ojson.GetBytes(body, "max_new_tokens").Exists() || ojson.GetBytes(body, "request.max_new_tokens").Exists() {
		return nil, types.NewMediaParameterError("max_new_tokens", "media_output_limit_unsupported")
	}
	var wire VolcengineTTSRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	if wire.Request.Model != public.Model || wire.Request.Text == "" || wire.Audio.Rate != 24000 || wire.Audio.VoiceType == "" {
		return nil, fmt.Errorf("invalid Volcengine speech parameters")
	}
	if wire.Request.Operation != "query" && wire.Request.Operation != "submit" || wire.Request.Operation == "query" && !p.query || wire.Request.Operation == "submit" && !p.submit || wire.Audio.Encoding == "wav" && wire.Request.Operation != "query" {
		return nil, types.NewMediaParameterError("response_format", "media_format_unsupported")
	}
	if _, err := (speechRequestBody{}).MinimumAudioOutputTokens(body); err != nil {
		return nil, err
	}
	if public.SampleRate != nil && *public.SampleRate != wire.Audio.Rate {
		return nil, fmt.Errorf("Volcengine sample rate override cannot be preserved")
	}
	public.SampleRate = common.GetPointer(wire.Audio.Rate)
	public.Input, public.Voice = wire.Request.Text, wire.Audio.VoiceType
	switch wire.Audio.Encoding {
	case "mp3", "wav", "pcm":
		public.ResponseFormat = wire.Audio.Encoding
	case "ogg_opus":
		public.ResponseFormat = "opus"
	default:
		return nil, fmt.Errorf("unsupported Volcengine audio encoding")
	}
	return &public, nil
}

func (speechRequestBody) MinimumAudioOutputTokens(body []byte) (int, error) {
	var wire VolcengineTTSRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return 0, err
	}
	// V1 shares transports across voice families; select controls by exact voice ID.
	// https://www.volcengine.com/docs/6561/79823
	// https://www.volcengine.com/docs/doubao-voice/http-one-timecompositenon-streaming-interface-v1-not-recommended
	// Published 1.0 and small-model tone lists define the exact IDs below:
	// https://docs.volcengine.com/docs/doubao-voice/tone-list-3?lang=zh
	// https://docs.volcengine.com/docs/doubao-voice/tone-list?lang=zh
	minSpeed, maxSpeed, largeModel := 1.0, 1.0, false
	switch wire.Audio.VoiceType {
	case "zh_male_M392_conversation_wvae_bigtts", "zh_male_lengkugege_emo_v2_mars_bigtts", "zh_female_tianxinxiaomei_emo_v2_mars_bigtts",
		"zh_male_guangzhoudege_emo_mars_bigtts", "zh_male_jingqiangkanye_emo_mars_bigtts", "zh_female_linjuayi_emo_v2_mars_bigtts",
		"zh_male_yourougongzi_emo_v2_mars_bigtts", "zh_male_ruyayichen_emo_v2_mars_bigtts", "zh_male_junlangnanyou_emo_v2_mars_bigtts",
		"zh_male_beijingxiaoye_emo_v2_mars_bigtts", "zh_female_roumeinvyou_emo_v2_mars_bigtts", "zh_male_yangguangqingnian_emo_v2_mars_bigtts",
		"zh_female_meilinvyou_emo_v2_mars_bigtts", "zh_female_shuangkuaisisi_emo_v2_mars_bigtts", "zh_male_shenyeboke_emo_v2_mars_bigtts",
		"en_female_candice_emo_v2_mars_bigtts", "zh_female_yingyujiaoyu_mars_bigtts", "zh_female_qinqienvsheng_moon_bigtts",
		"zh_male_qingyiyuxuan_mars_bigtts", "zh_male_xudong_conversation_wvae_bigtts", "en_male_jason_conversation_wvae_bigtts",
		"zh_female_tianmeitaozi_mars_bigtts", "zh_female_qingxinnvsheng_mars_bigtts", "zh_female_zhixingnvsheng_mars_bigtts",
		"zh_male_qingshuangnanda_mars_bigtts", "zh_female_linjianvhai_moon_bigtts", "zh_male_yuanboxiaoshu_moon_bigtts",
		"zh_male_yangguangqingnian_moon_bigtts", "zh_female_tianmeixiaoyuan_moon_bigtts", "zh_female_qingchezizi_moon_bigtts",
		"zh_male_jieshuoxiaoming_moon_bigtts", "zh_female_kailangjiejie_moon_bigtts", "zh_male_linjiananhai_moon_bigtts",
		"zh_female_tianmeiyueyue_moon_bigtts", "zh_female_xinlingjitang_moon_bigtts", "zh_male_wenrouxiaoge_mars_bigtts",
		"zh_female_cancan_mars_bigtts", "zh_female_shuangkuaisisi_moon_bigtts", "zh_male_wennuanahu_moon_bigtts",
		"zh_male_shaonianzixin_moon_bigtts", "zh_female_yueyunv_mars_bigtts", "zh_male_yuzhouzixuan_moon_bigtts",
		"zh_female_daimengchuanmei_moon_bigtts", "zh_male_guangxiyuanzhou_moon_bigtts", "zh_male_zhoujielun_emo_v2_mars_bigtts",
		"zh_female_wanwanxiaohe_moon_bigtts", "zh_female_wanqudashu_moon_bigtts", "zh_male_guozhoudege_moon_bigtts",
		"zh_male_haoyuxiaoge_moon_bigtts", "zh_male_beijingxiaoye_moon_bigtts", "zh_male_jingqiangkanye_moon_bigtts",
		"zh_female_meituojieer_moon_bigtts", "zh_male_naiqimengwa_mars_bigtts", "zh_female_popo_mars_bigtts",
		"zh_female_gaolengyujie_moon_bigtts", "zh_male_aojiaobazong_moon_bigtts", "zh_female_meilinvyou_moon_bigtts",
		"zh_male_shenyeboke_moon_bigtts", "zh_female_sajiaonvyou_moon_bigtts", "zh_female_yuanqinvyou_moon_bigtts",
		"zh_male_dongfanghaoran_moon_bigtts", "zh_male_M100_conversation_wvae_bigtts", "en_male_adam_mars_bigtts",
		"en_female_amanda_mars_bigtts", "en_male_jackson_mars_bigtts", "en_male_smith_mars_bigtts",
		"en_female_anna_mars_bigtts", "en_female_sarah_mars_bigtts", "en_male_dryw_mars_bigtts",
		"multi_female_maomao_conversation_wvae_bigtts", "multi_male_M100_conversation_wvae_bigtts", "multi_female_sophie_conversation_wvae_bigtts",
		"multi_male_xudong_conversation_wvae_bigtts", "multi_zh_male_youyoujunzi_moon_bigtts", "multi_female_gaolengyujie_moon_bigtts",
		"multi_male_jingqiangkanye_moon_bigtts", "multi_female_shuangkuaisisi_moon_bigtts", "multi_male_wanqudashu_moon_bigtts",
		"zh_female_maomao_conversation_wvae_bigtts", "zh_female_wenrouxiaoya_moon_bigtts", "zh_male_tiancaitongsheng_mars_bigtts",
		"zh_male_sunwukong_mars_bigtts", "zh_male_xionger_mars_bigtts", "zh_female_peiqi_mars_bigtts",
		"zh_female_wuzetian_mars_bigtts", "zh_female_gujie_mars_bigtts", "zh_female_yingtaowanzi_mars_bigtts",
		"zh_male_chunhui_mars_bigtts", "zh_female_shaoergushi_mars_bigtts", "zh_male_silang_mars_bigtts",
		"zh_female_qiaopinvsheng_mars_bigtts", "zh_male_lanxiaoyang_mars_bigtts", "zh_male_dongmanhaimian_mars_bigtts",
		"zh_female_jitangmeimei_mars_bigtts", "zh_female_tiexinnvsheng_mars_bigtts", "zh_female_mengyatou_mars_bigtts",
		"zh_male_changtianyi_mars_bigtts", "zh_male_ruyaqingnian_mars_bigtts", "zh_male_baqiqingshu_mars_bigtts",
		"zh_male_qingcang_mars_bigtts", "zh_male_yangguangqingnian_mars_bigtts", "zh_female_gufengshaoyu_mars_bigtts",
		"zh_female_wenroushunv_mars_bigtts", "zh_male_fanjuanqingnian_mars_bigtts":
		minSpeed, maxSpeed, largeModel = 0.1, 2, true
	case "BV700_V2_streaming", "BV705_streaming", "BV701_V2_streaming",
		"BV001_V2_streaming", "BV700_streaming", "BV406_V2_streaming",
		"BV406_streaming", "BV407_V2_streaming", "BV407_streaming",
		"BV001_streaming", "BV002_streaming", "BV701_streaming",
		"BV123_streaming", "BV120_streaming", "BV119_streaming",
		"BV115_streaming", "BV107_streaming", "BV100_streaming",
		"BV104_streaming", "BV004_streaming", "BV113_streaming",
		"BV102_streaming", "BV405_streaming", "BV007_streaming",
		"BV009_streaming", "BV419_streaming", "BV415_streaming",
		"BV008_streaming", "BV408_streaming", "BV426_streaming",
		"BV428_streaming", "BV403_streaming", "BV158_streaming",
		"BV157_streaming", "BV410_streaming", "BV411_streaming",
		"BV437_streaming", "BV412_streaming", "BV159_streaming",
		"BV418_streaming", "BV142_streaming", "BV143_streaming",
		"BV056_streaming", "BV005_streaming", "BV064_streaming",
		"BV051_streaming", "BV063_streaming", "BV417_streaming",
		"BV050_streaming", "BV061_streaming", "BV401_streaming",
		"BV402_streaming", "BV006_streaming", "BV011_streaming",
		"BV012_streaming", "BV034_streaming", "BV033_streaming",
		"BV511_streaming", "BV505_streaming", "BV138_streaming",
		"BV027_streaming", "BV502_streaming", "BV503_streaming",
		"BV504_streaming", "BV421_streaming", "BV702_streaming",
		"BV506_streaming", "BV040_streaming", "BV516_streaming",
		"BV520_streaming", "BV521_streaming", "BV522_streaming",
		"BV524_streaming", "BV531_streaming", "BV530_streaming",
		"BV065_streaming", "BV021_streaming", "BV020_streaming",
		"BV704_streaming", "BV210_streaming", "BV217_streaming",
		"BV213_streaming", "BV025_streaming", "BV227_streaming",
		"BV026_streaming", "BV424_streaming", "BV212_streaming",
		"BV019_streaming", "BV221_streaming", "BV423_streaming",
		"BV214_streaming", "BV226_streaming", "BV216_streaming":
		minSpeed, maxSpeed = 0.2, 3
	}
	speed := wire.Audio.SpeedRatio
	if !ojson.GetBytes(body, "audio.speed_ratio").Exists() {
		speed = 1
	}
	if math.IsNaN(speed) || math.IsInf(speed, 0) || speed < minSpeed || speed > maxSpeed {
		return 0, types.NewMediaParameterError("speed", "media_control_unsupported")
	}
	if wire.Request.SilenceDuration == nil {
		if wire.Request.EnableTrailingSilenceAudio != nil && *wire.Request.EnableTrailingSilenceAudio {
			return 0, fmt.Errorf("Volcengine trailing silence requires an explicit duration")
		}
		return 0, nil
	}
	if !largeModel || wire.Request.EnableTrailingSilenceAudio == nil || !*wire.Request.EnableTrailingSilenceAudio {
		return 0, fmt.Errorf("Volcengine trailing silence requires a supported voice and enabled switch")
	}
	milliseconds := *wire.Request.SilenceDuration
	if math.IsNaN(milliseconds) || math.IsInf(milliseconds, 0) || milliseconds < 0 || milliseconds > 30_000 {
		return 0, fmt.Errorf("Volcengine trailing silence exceeds the request limit")
	}
	seconds := milliseconds / 1000
	tokens, clamp := common.AudioDurationTokens(seconds)
	if clamp != nil {
		return 0, fmt.Errorf("Volcengine silence duration exceeds the request limit")
	}
	return tokens, nil
}
