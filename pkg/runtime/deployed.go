package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

// A deploy builds and zips every function, then lets Pulumi find out that most
// of the zips are the ones it already uploaded. A runtime that can tell what a
// build would produce without running it implements Signer, and a function
// whose signature is the one recorded in the state at its last deploy is not
// built at all: its code object keeps the zip it has.

// Signer is implemented by a runtime that can sign a build before running it.
// Two builds with the same signature must produce the same files.
type Signer interface {
	// Sign returns the signature of the build the input asks for and the
	// handler that build reports. An empty signature means the runtime cannot
	// vouch for this build, and it is built as usual.
	Sign(ctx context.Context, input *BuildInput) (signature string, handler string, err error)
}

// deployedCode is what the state says about a function's last deployed code.
type deployedCode struct {
	// signature of the build that produced the zip
	signature string
	// key of the zip in the asset bucket
	key string
	// path the zip was read from, which is where the provider looks for it
	path string
	// why the zip cannot be kept whatever the build signs, "" when it can
	problem string
}

const (
	functionType   = "sst:aws:Function"
	codeObjectType = "aws:s3/bucketObjectv2:BucketObjectv2"
)

// LoadDeployed reads, from the state file a deploy starts from, the signature
// and the code object of every function. It has to run before Pulumi does,
// since Pulumi rewrites that file as it goes. Any function it cannot read with
// certainty is left out, and so gets built.
func (c *Collection) LoadDeployed(statePath string) {
	// A deploy signs what the sources are when it starts. A process that
	// deploys more than once, as sst dev does, must not sign the second
	// deploy with what the first one read.
	for _, r := range c.runtimes {
		if forgetter, ok := r.(interface{ ForgetSignatures() }); ok {
			forgetter.ForgetSignatures()
		}
	}
	deployed, err := readDeployed(statePath)
	if err != nil {
		slog.Info("no deployed signatures", "err", err)
	}
	c.deployedMut.Lock()
	c.deployed = deployed
	c.deployedMut.Unlock()
}

func readDeployed(statePath string) (map[string]deployedCode, error) {
	file, err := os.Open(statePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var untyped apitype.VersionedCheckpoint
	if err := json.NewDecoder(file).Decode(&untyped); err != nil {
		return nil, err
	}
	var checkpoint apitype.CheckpointV3
	if err := json.Unmarshal(untyped.Checkpoint, &checkpoint); err != nil {
		return nil, err
	}
	if checkpoint.Latest == nil {
		return nil, nil
	}

	type recorded struct {
		urn       string
		signature string
		key       string
	}
	functions := map[string]recorded{}
	// a name two functions share cannot say which one a build is for
	ambiguous := map[string]bool{}
	for _, res := range checkpoint.Latest.Resources {
		if res.Delete || string(res.Type) != functionType {
			continue
		}
		name := res.URN.Name()
		if _, seen := functions[name]; seen {
			ambiguous[name] = true
			continue
		}
		code, _ := res.Outputs["_code"].(map[string]any)
		signature, _ := code["signature"].(string)
		key, _ := code["key"].(string)
		if signature == "" || key == "" {
			continue
		}
		functions[name] = recorded{urn: string(res.URN), signature: signature, key: key}
	}

	result := map[string]deployedCode{}
	for name, fn := range functions {
		problem := "its code object is not in the state"
		if ambiguous[name] {
			problem = "another function has its name"
		}
		result[name] = deployedCode{signature: fn.signature, key: fn.key, problem: problem}
	}
	for _, res := range checkpoint.Latest.Resources {
		if res.Delete || string(res.Type) != codeObjectType {
			continue
		}
		name, ok := strings.CutSuffix(res.URN.Name(), "Code")
		if !ok || ambiguous[name] {
			continue
		}
		fn, ok := functions[name]
		if !ok || string(res.Parent) != fn.urn {
			continue
		}
		code := deployedCode{signature: fn.signature, key: fn.key}
		// The signature is recorded once the function runs the zip, the object
		// as soon as it is uploaded. When a deploy fails between the two they
		// disagree, and the signature no longer describes what is deployed.
		key, _ := res.Inputs["key"].(string)
		source, _ := res.Inputs["source"].(map[string]any)
		code.path, _ = source["path"].(string)
		switch {
		case key != fn.key:
			code.problem = "its zip is not the one the signature was recorded with"
		case code.path == "":
			code.problem = "its zip has no path in the state"
		}
		result[name] = code
	}
	return result, nil
}

// sign returns the signature of the build the input asks for, or "" with the
// reason when the build has to run unsigned: the caller did not ask, the
// runtime cannot sign, or the build puts in the zip something the signature
// does not cover.
func (c *Collection) sign(ctx context.Context, input *BuildInput) (signature string, handler string, unsigned string) {
	switch {
	case input.Dev:
		return "", "", "a dev build"
	case input.IsContainer:
		return "", "", "a container"
	case input.Bundle != "":
		return "", "", "a bundle that is not built here"
	case input.Code == "":
		return "", "", "the caller cannot keep the deployed zip"
	case len(input.CopyFiles) > 0:
		return "", "", "files are copied into the zip"
	}
	r, ok := c.Runtime(input.Runtime)
	if !ok {
		return "", "", "no runtime"
	}
	signer, ok := r.(Signer)
	if !ok {
		return "", "", "the runtime does not sign its builds"
	}
	built, handler, err := signer.Sign(ctx, input)
	if err != nil {
		return "", "", "the runtime could not sign it: " + err.Error()
	}
	if built == "" {
		return "", "", "the runtime cannot vouch for it"
	}
	// what Build adds to the runtime's files: the linked resources, encrypted
	links, err := json.Marshal(input.Links)
	if err != nil {
		return "", "", "its links cannot be encoded"
	}
	hash := sha256.New()
	for _, part := range [][]byte{
		[]byte(built),
		[]byte(input.Runtime),
		[]byte(handler),
		input.Properties,
		[]byte(input.EncryptionKey),
		links,
	} {
		hash.Write([]byte{byte(len(part) >> 24), byte(len(part) >> 16), byte(len(part) >> 8), byte(len(part))})
		hash.Write(part)
	}
	return hex.EncodeToString(hash.Sum(nil)), handler, ""
}

// decide says whether a build with this signature can be skipped. When it
// can, key is the zip the function already runs. When it cannot, reason says
// why. previous is the signature recorded at the last deploy, if any.
func (c *Collection) decide(input *BuildInput, signature string) (key string, previous string, reason string) {
	c.deployedMut.RLock()
	code, ok := c.deployed[input.FunctionID]
	c.deployedMut.RUnlock()
	switch {
	case !ok:
		return "", "", "nothing deployed with a signature yet"
	case code.signature != signature:
		return "", code.signature, "differs from the deployed one"
	case code.problem != "":
		return "", code.signature, "equals the deployed one, but " + code.problem
	// The provider reads the zip from the path the state recorded. The caller
	// leaves a stand-in at input.Code, so the two have to be the same file.
	case code.path != input.Code:
		return "", code.signature, "equals the deployed one, but its zip was read from another path"
	}
	return code.key, code.signature, ""
}

// decision is what signing a build led to.
type decision struct {
	// signature of the build, "" when it could not be signed
	signature string
	handler   string
	// key of the zip to keep, "" when the build has to run
	key string
	// signature recorded at the last deploy, if any
	previous string
	// why the build has to run
	reason string
}

// signAndDecide signs the build, decides, and writes the decision in the log,
// one line per function of a deploy.
func (c *Collection) signAndDecide(ctx context.Context, input *BuildInput) decision {
	signature, handler, unsigned := c.sign(ctx, input)
	if signature == "" {
		if !input.Dev {
			slog.Info("signature: none, building", "functionID", input.FunctionID, "reason", unsigned)
		}
		return decision{reason: "not signed: " + unsigned}
	}
	key, previous, reason := c.decide(input, signature)
	if key != "" {
		slog.Info("signature: equals the deployed one, not building", "functionID", input.FunctionID, "signature", signature)
		return decision{signature: signature, handler: handler, key: key, previous: previous}
	}
	slog.Info("signature: "+reason+", building", "functionID", input.FunctionID, "signature", signature, "deployed", previous)
	return decision{signature: signature, handler: handler, previous: previous, reason: reason}
}
