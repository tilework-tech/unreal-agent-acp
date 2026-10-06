// Package schemacheck validates ACP payloads against the official JSON schema.
// It exists for tests; production code never validates its own output.
package schemacheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tilework-tech/unreal-agent-acp/schema"
)

const v1URL = "https://agentclientprotocol.com/schema/v1.json"

var (
	compileOnce sync.Once
	compiler    *jsonschema.Compiler
	compileErr  error
	cacheMu     sync.Mutex
	cache       = map[string]*jsonschema.Schema{}
)

// V1 validates payload (JSON) against the v1 definition #/$defs/<definition>.
func V1(definition string, payload []byte) error {
	compileOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.V1))
		if err != nil {
			compileErr = err
			return
		}
		compiler = jsonschema.NewCompiler()
		compileErr = compiler.AddResource(v1URL, document)
	})
	if compileErr != nil {
		return fmt.Errorf("load ACP v1 schema: %w", compileErr)
	}
	cacheMu.Lock()
	compiled, exists := cache[definition]
	if !exists {
		var err error
		compiled, err = compiler.Compile(v1URL + "#/$defs/" + definition)
		if err != nil {
			cacheMu.Unlock()
			return fmt.Errorf("compile #/$defs/%s: %w", definition, err)
		}
		cache[definition] = compiled
	}
	cacheMu.Unlock()

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	return compiled.Validate(instance)
}

// V1Value marshals value and validates it against #/$defs/<definition>.
func V1Value(definition string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := V1(definition, encoded); err != nil {
		return fmt.Errorf("%s %s: %w", definition, encoded, err)
	}
	return nil
}
