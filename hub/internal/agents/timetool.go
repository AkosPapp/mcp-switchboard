package agents

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

const timeNowName = "switchboard.time.now"

var timeNowTool = &sbTool{
	name: timeNowName,
	desc: "Get the current date and time, optionally in another IANA time zone, shifted by a duration, or converted from one zone to another. Returns iso8601, unix seconds, weekday, tz and utc_offset. Use it instead of guessing the date. " +
		`Example: {"tz": "Europe/Berlin", "add": "48h"}`,
	schema: obj(nil, map[string]any{
		"tz":           typ("string", "IANA zone such as Europe/Berlin or America/New_York; default the hub's local zone"),
		"add":          typ("string", "duration to add to now, e.g. 90m, 36h or -24h (Go syntax, also accepts days like 2d)"),
		"convert_from": typ("string", "a time to convert, RFC3339 or 'YYYY-MM-DD HH:MM[:SS]' (the latter read in the zone given by from_tz)"),
		"from_tz":      typ("string", "IANA zone in which a zone-less convert_from is read; default UTC"),
		"convert_to":   typ("string", "IANA zone to convert convert_from into (used with convert_from)"),
	}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true},
	visible: func(store.Capabilities) bool { return true },
	run: func(_ *Manager, _ context.Context, _ *callCtx, args map[string]any) (any, error) {
		return timeNow(time.Now(), args)
	},
}

func init() { registerExtraTool(timeNowTool, nil) }

func loadZone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown time zone %q (use an IANA name like Europe/Berlin)", ErrInvalid, name)
	}
	return loc, nil
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		var n float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%g", &n); err == nil {
			return time.Duration(n * 24 * float64(time.Hour)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: bad duration %q (examples: 90m, 36h, -24h, 2d)", ErrInvalid, s)
	}
	return d, nil
}

func timeNow(now time.Time, args map[string]any) (any, error) {
	loc, err := loadZone(argStr(args, "tz"))
	if err != nil {
		return nil, err
	}
	t := now
	if from := strings.TrimSpace(argStr(args, "convert_from")); from != "" {
		to := strings.TrimSpace(argStr(args, "convert_to"))
		if to == "" {
			return nil, fmt.Errorf("%w: convert_from needs convert_to", ErrInvalid)
		}
		fl := time.UTC
		if z := strings.TrimSpace(argStr(args, "from_tz")); z != "" {
			if fl, err = loadZone(z); err != nil {
				return nil, err
			}
		}
		parsed, perr := time.Parse(time.RFC3339, from)
		if perr != nil {
			for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
				if parsed, perr = time.ParseInLocation(layout, from, fl); perr == nil {
					break
				}
			}
		}
		if perr != nil {
			return nil, fmt.Errorf("%w: cannot parse convert_from %q", ErrInvalid, from)
		}
		t = parsed
		if loc, err = loadZone(to); err != nil {
			return nil, err
		}
	}
	if add := strings.TrimSpace(argStr(args, "add")); add != "" {
		d, err := parseDuration(add)
		if err != nil {
			return nil, err
		}
		t = t.Add(d)
	}
	t = t.In(loc)
	name, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return map[string]any{
		"iso8601":    t.Format(time.RFC3339),
		"unix":       t.Unix(),
		"weekday":    t.Weekday().String(),
		"tz":         loc.String(),
		"tz_abbrev":  name,
		"utc_offset": fmt.Sprintf("%s%02d:%02d", sign, off/3600, off%3600/60),
	}, nil
}
