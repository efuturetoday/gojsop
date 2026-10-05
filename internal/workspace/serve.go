package workspace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"sigs.k8s.io/yaml"

	"github.com/efuturetoday/gojsop/internal/jslog"
)

// serveRequest is one line `gojsop serve --stdio` reads.
type serveRequest struct {
	ID       json.RawMessage  `json:"id"`
	Op       string           `json:"op"`
	Manifest string           `json:"manifest"`
	Input    json.RawMessage  `json:"input"`
	Cluster  []map[string]any `json:"cluster"`
	// Source, when set, is the script to run instead of the one the
	// manifest names (workspace.R11).
	Source *string `json:"source,omitempty"`
}

// serveError is the error of an answer; Kind is one of the Kind constants.
type serveError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// serveAnswer is one line `gojsop serve --stdio` writes.
type serveAnswer struct {
	ID      json.RawMessage  `json:"id"`
	Result  *Output          `json:"result,omitempty"`
	Console []jslog.Line     `json:"console"`
	Cluster []map[string]any `json:"cluster"`
	Error   *serveError      `json:"error,omitempty"`
}

// Serve answers one JSON request per line of r with one JSON line on w until
// r ends. A failing call, a bad line or an unloadable manifest is an error
// answer, never the end of the loop. Calls run one at a time.
// workspace.R4
func Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	in := bufio.NewReader(r)
	out := bufio.NewWriter(w)
	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if encErr := json.NewEncoder(out).Encode(serveOne(ctx, line)); encErr != nil {
				return encErr
			}
			if flushErr := out.Flush(); flushErr != nil {
				return flushErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// serveOne runs one request line and builds its answer.
// workspace.R5
func serveOne(ctx context.Context, line []byte) serveAnswer {
	ans := serveAnswer{ID: json.RawMessage("null"), Console: []jslog.Line{}, Cluster: []map[string]any{}}
	fail := func(kind string, err error) serveAnswer {
		ans.Error = &serveError{Kind: kind, Message: err.Error()}
		return ans
	}
	var req serveRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return fail(KindInput, fmt.Errorf("bad request line: %w", err))
	}
	if len(req.ID) > 0 {
		ans.ID = req.ID
	}
	ans.Cluster = nonNil(req.Cluster)

	var m *Manifest
	var err error
	if req.Source != nil {
		m, err = LoadWithSource(req.Manifest, []byte(*req.Source))
	} else {
		m, err = Load(req.Manifest)
	}
	if err != nil {
		return fail(KindInput, err)
	}
	call := Input{Cluster: req.Cluster}
	switch {
	case req.Op == "review" && m.Policy != nil:
		call.Request = &RequestSpec{}
		err = decodeStrict(req.Input, call.Request)
	case req.Op == "handle" && m.Hook != nil:
		call.Event = &EventSpec{}
		err = decodeStrict(req.Input, call.Event)
	case req.Op == "review":
		err = errors.New("review needs a policy manifest")
	case req.Op == "handle":
		err = errors.New("handle needs a hook manifest")
	default:
		err = fmt.Errorf("unknown op %q", req.Op)
	}
	if err != nil {
		return fail(KindInput, err)
	}

	res, err := m.Run(ctx, call)
	if err != nil {
		if errors.Is(err, errBuild) {
			return fail(KindScript, err)
		}
		return fail(KindInput, err)
	}
	ans.Console = nonNil(res.Console)
	ans.Cluster = nonNil(res.Cluster)
	if res.Error != "" {
		ans.Error = &serveError{Kind: res.Kind, Message: res.Error}
		return ans
	}
	shown := *res
	shown.Console, shown.Cluster = nil, nil
	ans.Result = &shown
	return ans
}

func decodeStrict(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return errors.New("request has no input")
	}
	if err := yaml.UnmarshalStrict(raw, into); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	return nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
