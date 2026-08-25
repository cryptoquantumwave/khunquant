package voice

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestDetectTranscriberBackendSelection(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *config.Config
		wantNil  bool
		wantName string
	}{
		{
			name:    "no config",
			cfg:     &config.Config{},
			wantNil: true,
		},
		{
			name: "voice model name selects audio model transcriber",
			cfg: &config.Config{
				Voice: config.VoiceConfig{ModelName: "voice-gemini"},
				ModelList: []config.ModelConfig{
					{
						ModelName: "voice-gemini",
						Model:     "gemini/gemini-2.5-flash",
						APIKey:    *config.NewSecureString("sk-gemini-model"),
					},
				},
			},
			wantName: "audio-model",
		},
		{
			name: "voice model name alias selects elevenlabs transcriber",
			cfg: &config.Config{
				Voice: config.VoiceConfig{ModelName: "my-asr-model"},
				ModelList: []config.ModelConfig{
					{
						ModelName: "my-asr-model",
						Model:     "elevenlabs/scribe_v1",
						APIKey:    *config.NewSecureString("sk_elevenlabs_test"),
					},
				},
			},
			wantName: "elevenlabs",
		},
		{
			name: "voice model name alias selects whisper transcriber for groq",
			cfg: &config.Config{
				Voice: config.VoiceConfig{ModelName: "my-asr-model"},
				ModelList: []config.ModelConfig{
					{
						ModelName: "my-asr-model",
						Model:     "groq/whisper-large-v3",
						APIKey:    *config.NewSecureString("sk-groq-model"),
					},
				},
			},
			wantName: "whisper",
		},
		{
			name: "openai whisper alias selects whisper transcriber",
			cfg: &config.Config{
				Voice: config.VoiceConfig{ModelName: "my-asr-model"},
				ModelList: []config.ModelConfig{
					{
						ModelName: "my-asr-model",
						Model:     "openai/whisper-1",
						APIKey:    *config.NewSecureString("sk-openai-model"),
					},
				},
			},
			wantName: "whisper",
		},
		{
			name: "whisper via model list fallback",
			cfg: &config.Config{
				ModelList: []config.ModelConfig{
					{ModelName: "openai", Model: "openai/gpt-4o", APIKey: *config.NewSecureString("sk-openai")},
					{
						ModelName: "groq",
						Model:     "groq/whisper-large-v3-turbo",
						APIKey:    *config.NewSecureString("sk-groq-model"),
					},
				},
			},
			wantName: "whisper",
		},
		{
			name: "voice model name alias selects non-gemini audio model transcriber",
			cfg: &config.Config{
				Voice: config.VoiceConfig{ModelName: "my-asr-model"},
				ModelList: []config.ModelConfig{
					{
						ModelName: "my-asr-model",
						Model:     "openai/gpt-4o-audio-preview",
						APIKey:    *config.NewSecureString("sk-openai"),
					},
				},
			},
			wantName: "audio-model",
		},
		{
			name: "voice model name selects azure audio model transcriber",
			cfg: &config.Config{
				Voice: config.VoiceConfig{ModelName: "voice-azure-audio"},
				ModelList: []config.ModelConfig{
					{
						ModelName: "voice-azure-audio",
						Model:     "azure/my-audio-deployment",
						APIKey:    *config.NewSecureString("sk-azure"),
						APIBase:   "https://example.openai.azure.com",
					},
				},
			},
			wantName: "audio-model",
		},
		{
			name: "voice model name with non openai compatible protocol does not select audio model transcriber",
			cfg: &config.Config{
				Voice: config.VoiceConfig{ModelName: "voice-anthropic"},
				ModelList: []config.ModelConfig{
					{
						ModelName: "voice-anthropic",
						Model:     "anthropic/claude-sonnet-4.6",
						APIKey:    *config.NewSecureString("sk-anthropic"),
					},
				},
			},
			wantNil: true,
		},
		{
			name: "groq model list entry without key is skipped",
			cfg: &config.Config{
				ModelList: []config.ModelConfig{
					{Model: "groq/whisper-large-v3"},
				},
			},
			wantNil: true,
		},
		{
			name: "model list entry without key is skipped",
			cfg: &config.Config{
				ModelList: []config.ModelConfig{
					{
						ModelName: "groq",
						Model:     "groq/whisper-large-v3",
						APIKey:    *config.NewSecureString("sk-groq-model"),
					},
				},
			},
			wantName: "whisper",
		},
		{
			name: "missing voice model name config returns nil",
			cfg: &config.Config{
				Voice: config.VoiceConfig{ModelName: "missing"},
				ModelList: []config.ModelConfig{
					{
						ModelName: "other",
						Model:     "gemini/gemini-2.5-flash",
						APIKey:    *config.NewSecureString("sk-other-model"),
					},
				},
			},
			wantNil: true,
		},
		{
			name: "elevenlabs voice config key",
			cfg: &config.Config{
				ModelList: []config.ModelConfig{
					{Model: "elevenlabs/scribe_v1", APIKey: *config.NewSecureString("sk_elevenlabs_test")},
				},
			},
			wantName: "elevenlabs",
		},
		{
			name: "elevenlabs takes priority over groq model list",
			cfg: &config.Config{
				ModelList: []config.ModelConfig{
					{Model: "elevenlabs/scribe_v1", APIKey: *config.NewSecureString("sk_elevenlabs_test")},
					{
						ModelName: "groq",
						Model:     "groq/llama-3.3-70b",
						APIKey:    *config.NewSecureString("sk-groq-model"),
					},
				},
			},
			wantName: "elevenlabs",
		},
		{
			name: "voice model name takes priority over elevenlabs",
			cfg: &config.Config{
				Voice: config.VoiceConfig{
					ModelName: "voice-gemini",
				},
				ModelList: []config.ModelConfig{
					{Model: "elevenlabs/scribe_v1", APIKey: *config.NewSecureString("sk_elevenlabs_test")},
					{
						ModelName: "voice-gemini",
						Model:     "gemini/gemini-2.5-flash",
						APIKey:    *config.NewSecureString("sk-gemini-model"),
					},
				},
			},
			wantName: "audio-model",
		},
		{
			name: "direct elevenlabs api key takes priority over model list",
			cfg: &config.Config{
				Voice: config.VoiceConfig{
					ElevenLabsAPIKey: "sk_elevenlabs_direct",
				},
				ModelList: []config.ModelConfig{
					{
						ModelName: "groq",
						Model:     "groq/whisper-large-v3",
						APIKey:    *config.NewSecureString("sk-groq-model"),
					},
				},
			},
			wantName: "elevenlabs",
		},
		{
			name: "groq provider key fallback",
			cfg: &config.Config{
				Providers: config.ProvidersConfig{
					Groq: config.ProviderConfig{APIKey: "sk-groq-direct"},
				},
			},
			wantName: "groq",
		},
		{
			name: "groq llama via model list without whisper in name uses groq backend",
			cfg: &config.Config{
				ModelList: []config.ModelConfig{
					{
						Model: "groq/llama-3.3-70b",
						APIKey: *config.NewSecureString("sk-groq-model"),
					},
				},
			},
			wantName: "groq",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := DetectTranscriber(tc.cfg)
			if tc.wantNil {
				if tr != nil {
					t.Errorf("DetectTranscriber() = %v, want nil", tr)
				}
				return
			}
			if tr == nil {
				t.Fatal("DetectTranscriber() = nil, want non-nil")
			}
			if got := tr.Name(); got != tc.wantName {
				t.Errorf("Name() = %q, want %q", got, tc.wantName)
			}
		})
	}
}
