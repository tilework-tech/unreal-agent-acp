// Derived from github.com/coder/acp-go-sdk cmd/generate@v0.13.5 (Apache-2.0).
// Modified by Tilework Tech Inc.; see tools/acpgen/README.md.

// Command acpgen generates ACP Go types from schema.json and meta.json.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tilework-tech/unreal-agent-acp/tools/acpgen/internal/emit"
	"github.com/tilework-tech/unreal-agent-acp/tools/acpgen/internal/load"
)

func main() {
	var schemaDirFlag string
	var outDirFlag string
	flag.StringVar(&schemaDirFlag, "schema", "", "path to schema directory containing schema.json and meta.json")
	flag.StringVar(&outDirFlag, "out", "", "output directory for generated go files")
	flag.StringVar(&emit.PackageName, "package", "acp", "Go package name for generated files")
	flag.Parse()

	schemaDir, outDir := schemaDirFlag, outDirFlag
	if schemaDir == "" || outDir == "" {
		fmt.Fprintln(os.Stderr, "acpgen: -schema and -out are required")
		os.Exit(2)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		panic(err)
	}

	meta, err := load.ReadMeta(schemaDir)
	if err != nil {
		panic(err)
	}

	schema, err := load.ReadSchema(schemaDir)
	if err != nil {
		panic(err)
	}

	unstableMeta, unstableMetaFound, err := load.ReadMetaUnstable(schemaDir)
	if err != nil {
		panic(err)
	}
	unstableSchema, unstableSchemaFound, err := load.ReadSchemaUnstable(schemaDir)
	if err != nil {
		panic(err)
	}
	if unstableMetaFound != unstableSchemaFound {
		panic(fmt.Sprintf("unstable schema/meta mismatch: meta found=%v schema found=%v", unstableMetaFound, unstableSchemaFound))
	}
	if unstableMetaFound {
		mergedMeta, mergedSchema, err := load.MergeStableAndUnstable(meta, schema, unstableMeta, unstableSchema)
		if err != nil {
			panic(err)
		}
		meta = mergedMeta
		schema = mergedSchema
	}

	if err := emit.WriteConstantsJen(outDir, meta); err != nil {
		panic(err)
	}

	if err := emit.WriteTypesJen(outDir, schema, meta); err != nil {
		panic(err)
	}

	// Emit helpers after types so they can reference generated structs.
	if err := emit.WriteHelpersJen(outDir, schema, meta); err != nil {
		panic(err)
	}
}
