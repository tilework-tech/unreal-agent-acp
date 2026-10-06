package unrealrt

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

const metadataSuffix = ".acp.json"

// metadata is what the session store does not record: it lives in a sidecar
// file next to <id>.session.jsonl until upstream stores session metadata.
type metadata struct {
	Cwd             string
	Title           string              `json:",omitzero"`
	Model           string              `json:",omitzero"`
	ReasoningEffort llm.ReasoningEffort `json:",omitzero"`
	CreatedAt       time.Time
}

func (runtime *Runtime) metadataPath(id session.ID) string {
	return filepath.Join(runtime.directory, string(id)+metadataSuffix)
}

func (runtime *Runtime) loadMetadata(id session.ID) (metadata, error) {
	encoded, err := os.ReadFile(runtime.metadataPath(id))
	if err != nil {
		return metadata{}, err
	}
	var meta metadata
	if err := json.Unmarshal(encoded, &meta); err != nil {
		return metadata{}, fmt.Errorf("decode session %q metadata: %w", id, err)
	}
	return meta, nil
}

func (runtime *Runtime) saveMetadata(id session.ID, meta metadata) error {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode session %q metadata: %w", id, err)
	}
	temporary, err := os.CreateTemp(runtime.directory, ".acp-*.tmp")
	if err != nil {
		return fmt.Errorf("write session %q metadata: %w", id, err)
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("write session %q metadata: %w", id, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write session %q metadata: %w", id, err)
	}
	if err := os.Rename(temporary.Name(), runtime.metadataPath(id)); err != nil {
		return fmt.Errorf("write session %q metadata: %w", id, err)
	}
	return nil
}
