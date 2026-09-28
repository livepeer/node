package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const module = "github.com/livepeer/node/"

func TestImportLattice(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(here), ".."))
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "vendor" || entry.Name() == ".git" || entry.Name() == "bin" || entry.Name() == ".tools" || entry.Name() == ".gocache" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			dep, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err, rel)
			require.Empty(t, forbiddenImport(filepath.ToSlash(rel), dep), "%s imports forbidden package %s", rel, dep)
		}
		return nil
	})
	require.NoError(t, err)
}

func forbiddenImport(file, dep string) string {
	for _, prefix := range []string{
		"github.com/livepeer/go-livepeer", "github.com/livepeer/lpms",
		"google.golang.org/grpc", "github.com/golang/protobuf",
	} {
		if strings.HasPrefix(dep, prefix) {
			return "legacy, media or gRPC dependency"
		}
	}
	if strings.HasPrefix(dep, "google.golang.org/protobuf") && !strings.HasPrefix(file, "signercompat/") {
		return "Protobuf runtime outside signercompat"
	}
	if !strings.HasPrefix(dep, module) {
		return ""
	}
	local := strings.TrimPrefix(dep, module)
	// A real HTTP compatibility test may compose component handlers without
	// creating a production dependency between those components.
	if file == "orchestrator/payment_test.go" && local == "signer" {
		return ""
	}
	if strings.HasPrefix(file, "cmd/livepeer/") && local != "version" {
		return "dispatcher imports component implementation"
	}
	for _, component := range []string{"orchestrator", "signer", "chain"} {
		if local == component || strings.HasPrefix(local, component+"/") {
			if strings.HasPrefix(file, "cmd/livepeer-"+component+"/") || strings.HasPrefix(file, component+"/") {
				continue
			}
			return "cross-component or shared-to-component import"
		}
	}
	if strings.HasPrefix(local, "signercompat") && !strings.HasPrefix(file, "signer/") && !strings.HasPrefix(file, "pm/") {
		return "signer compatibility import outside signer"
	}
	if (local == "pm" || strings.HasPrefix(local, "pm/")) && (strings.HasPrefix(file, "chain/") || strings.HasPrefix(file, "eth/")) {
		return "chain or Ethereum package imports payment implementation"
	}
	return ""
}
