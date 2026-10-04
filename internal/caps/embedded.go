// Code generated from opencode's catalog mirror for the free models
// the gateway serves. The live table is refreshed through a tor
// lane; this is the offline fallback.

package caps

func embedded() map[string]Entry {
	return map[string]Entry{
		"big-pickle": Entry{
			Name:       "Big Pickle",
			Reasoning:  true,
			ToolCall:   true,
			Attachment: false,
			Limit:      Limit{Context: 200000, Output: 32000},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text"}, Output: []string{"text"}},
		},
		"deepseek-v4-flash-free": Entry{
			Name:      "DeepSeek V4 Flash Free",
			Reasoning: true,
			ReasoningOptions: []Option{
				Option{Type: "effort", Values: []string{"low", "high", "max"}},
			},
			ToolCall:   true,
			Attachment: false,
			Limit:      Limit{Context: 200000, Output: 128000},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text"}, Output: []string{"text"}},
		},
		"muse-spark-1.3-contributor-free": Entry{
			Name:      "Muse Spark 1.3 Free",
			Reasoning: true,
			ReasoningOptions: []Option{
				Option{Type: "effort", Values: []string{"minimal", "low", "medium", "high", "xhigh"}},
			},
			ToolCall:   true,
			Attachment: true,
			Limit:      Limit{Context: 1048576, Output: 131072},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text", "image", "video", "pdf", "audio"}, Output: []string{"text"}},
		},
		"muse-spark-1.2-contributor-free": Entry{
			Name:      "Muse Spark 1.2 Free",
			Reasoning: true,
			ReasoningOptions: []Option{
				Option{Type: "effort", Values: []string{"minimal", "low", "medium", "high", "xhigh"}},
			},
			ToolCall:   true,
			Attachment: true,
			Limit:      Limit{Context: 1048576, Output: 131072},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text", "image", "video", "pdf", "audio"}, Output: []string{"text"}},
		},
		"mimo-v2.6-flash-free": Entry{
			Name:       "MiMo-V2.6-Flash Free",
			Reasoning:  true,
			ToolCall:   true,
			Attachment: true,
			Limit:      Limit{Context: 200000, Output: 32000},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text", "image", "audio", "video"}, Output: []string{"text"}},
		},
		"space-bunny-free": Entry{
			Name:      "Space Bunny Free",
			Reasoning: true,
			ReasoningOptions: []Option{
				Option{Type: "effort", Values: []string{"low", "medium", "high", "xhigh", "max"}},
			},
			ToolCall:   true,
			Attachment: true,
			Limit:      Limit{Context: 1048576, Output: 524288},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text", "image", "video"}, Output: []string{"text"}},
		},
		"longcat-2.5-preview-free": Entry{
			Name:      "LongCat 2.5 Preview Free",
			Reasoning: true,
			ReasoningOptions: []Option{
				Option{Type: "toggle", Values: []string{}},
			},
			ToolCall:   true,
			Attachment: true,
			Limit:      Limit{Context: 1000000, Output: 131072},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
		},
		"mimo-v2.5-free": Entry{
			Name:       "MiMo V2.5 Free",
			Reasoning:  true,
			ToolCall:   true,
			Attachment: true,
			Limit:      Limit{Context: 200000, Output: 32000},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text", "image", "audio", "video"}, Output: []string{"text"}},
		},
		"ling-3.0-flash-fin-free": Entry{
			Name:      "Ling 3.0 Flash Fin Free",
			Reasoning: true,
			ReasoningOptions: []Option{
				Option{Type: "toggle", Values: []string{}},
			},
			ToolCall:   true,
			Attachment: false,
			Limit:      Limit{Context: 262144, Output: 32768},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text"}, Output: []string{"text"}},
		},
		"nemotron-3-ultra-free": Entry{
			Name:       "Nemotron 3 Ultra Free",
			Reasoning:  true,
			ToolCall:   true,
			Attachment: false,
			Limit:      Limit{Context: 1000000, Output: 128000},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text"}, Output: []string{"text"}},
		},
		"nemotron-3.5-lightning-free": Entry{
			Name:       "Nemotron 3.5 Lightning Free",
			Reasoning:  true,
			ToolCall:   true,
			Attachment: false,
			Limit:      Limit{Context: 262144, Output: 262144},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text"}, Output: []string{"text"}},
		},
		"fledge-alpha-free": Entry{
			Name:      "Fledge Alpha Free",
			Reasoning: true,
			ReasoningOptions: []Option{
				Option{Type: "effort", Values: []string{"low", "high", "max"}},
			},
			ToolCall:   true,
			Attachment: true,
			Limit:      Limit{Context: 1048576, Output: 131072},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
		},
		"ling-3.1-flash-free": Entry{
			Name:      "Ling 3.1 Flash Free",
			Reasoning: true,
			ReasoningOptions: []Option{
				Option{Type: "toggle", Values: []string{}},
			},
			ToolCall:   true,
			Attachment: false,
			Limit:      Limit{Context: 262144, Output: 32768},
			Cost:       Cost{Input: 0, Output: 0},
			Modalities: Modalities{Input: []string{"text"}, Output: []string{"text"}},
		},
	}
}
