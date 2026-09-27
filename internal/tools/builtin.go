package tools

import (
	"context"
	"encoding/json"
	"time"
)

// Builtin provides small utility tools that need no configuration.
type Builtin struct{ name string }

func NewBuiltin(name string) *Builtin { return &Builtin{name: name} }

func (b *Builtin) Name() string { return b.name }
func (b *Builtin) Close() error { return nil }

func (b *Builtin) Tools(context.Context) ([]Tool, error) {
	return []Tool{{
		Local:       "now",
		Description: "Returns the current date and time. Optionally in a given IANA time zone (e.g. Europe/Berlin).",
		Schema:      Schema(map[string]any{"timezone": Prop("string", "IANA time zone name, default UTC")}),
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Timezone string `json:"timezone"`
			}
			if err := Decode(args, &a); err != nil {
				return "", err
			}
			loc := time.UTC
			if a.Timezone != "" {
				l, err := time.LoadLocation(a.Timezone)
				if err != nil {
					return "", err
				}
				loc = l
			}
			now := time.Now().In(loc)
			return JSON(map[string]string{"iso": now.Format(time.RFC3339), "weekday": now.Weekday().String(), "timezone": loc.String()}), nil
		},
	}}, nil
}
