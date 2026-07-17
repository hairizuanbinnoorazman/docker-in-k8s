package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/docker/go-units"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

type preparedBuildContext struct {
	root             string
	dockerfile       string
	ignorePath       string
	matcher          *patternmatcher.PatternMatcher
	dockerfileSource string
	cleanup          func()
	excludedPaths    map[string]struct{}
}

type buildRegistryOptions struct {
	insecure   bool
	authSecret string
	caSecret   string
	cachePVC   string
	secrets    bool
}

type buildSecret struct {
	id     string
	data   []byte
	source string
}

type buildSSH struct {
	id     string
	data   []byte
	source string
}

type buildNamedContext struct {
	name    string
	context *preparedBuildContext
}

type buildOutput struct {
	kind       string
	spec       string
	clientPath string
	podPath    string
}

type buildFrontendOptions struct {
	buildArgs     []string
	target        string
	labels        []string
	platforms     []string
	noCache       bool
	noCacheFilter []string
	pull          bool
	cacheFrom     []string
	cacheTo       []string
	secrets       []buildSecret
	ssh           []buildSSH
	network       string
	addHosts      []string
	shmSize       int64
	namedContexts []buildNamedContext
	outputs       []buildOutput
	attests       []string
	call          string
	debug         bool
}

type buildResultOptions struct {
	progress     string
	quiet        bool
	iidFile      string
	metadataFile string
}

const buildkitImage = "moby/buildkit:v0.30.0-rootless"

func newBuildCommand(opts *options) *cobra.Command {
	var tags, buildArgs, labels, platforms, noCacheFilter, cacheFrom, cacheTo, secretSpecs, sshSpecs []string
	var addHosts, allows, securityOpts, resourceSpecs, ulimits, namedContextSpecs, outputSpecs, annotations, attestSpecs []string
	var dockerfile, buildNamespace, registrySecret, registryCASecret, cachePVC, target string
	var networkMode, cgroupParent, builder, policy, isolation, shmSize, provenance, sbom, call string
	var progress, iidFile, metadataFile string
	var keep, push, noCache, pull, load, squash, check, debug bool
	var registryInsecure, quiet bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "build [flags] PATH",
		Short: "Build and push an image with BuildKit on Kubernetes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(allows) > 0 {
				return fmt.Errorf("--allow is disabled: dockube does not grant host networking, privileged security, or device entitlements")
			}
			if cgroupParent != "" {
				return fmt.Errorf("--cgroup-parent is unsupported: dockube builds cannot select host cgroups")
			}
			if load {
				return fmt.Errorf("--load is unsupported: dockube has no Docker Engine image store; use --push")
			}
			if builder != "" && builder != "default" {
				return fmt.Errorf("--builder=%s is unsupported: dockube currently provides only its default Kubernetes builder", builder)
			}
			if policy != "" {
				return fmt.Errorf("--policy is unsupported until dockube defines an administrator-controlled policy mechanism")
			}
			if isolation != "" || len(securityOpts) > 0 || squash {
				return fmt.Errorf("--isolation, --security-opt, and --squash are unsupported Windows or legacy-builder options")
			}
			if len(resourceSpecs) > 0 {
				return fmt.Errorf("--resource is unsupported: BuildKit Job resources are administrator-defined Kubernetes requests and limits")
			}
			if len(ulimits) > 0 {
				return fmt.Errorf("--ulimit is unsupported: dockube does not change node or container-runtime process limits")
			}
			shmBytes, err := normalizeShmSize(shmSize)
			if err != nil {
				return err
			}
			network, err := normalizeBuildNetwork(networkMode)
			if err != nil {
				return err
			}
			hosts, err := normalizeBuildHosts(addHosts)
			if err != nil {
				return err
			}
			resultOptions, err := normalizeBuildResultOptions(progress, quiet, iidFile, metadataFile)
			if err != nil {
				return err
			}
			callMode, err := normalizeBuildCall(call, check)
			if err != nil {
				return err
			}
			outputs, err := normalizeBuildOutputs(outputSpecs, push, tags, registryInsecure, callMode != "build")
			if err != nil {
				return err
			}
			outputs, err = applyBuildAnnotations(outputs, annotations)
			if err != nil {
				return err
			}
			attests, err := normalizeBuildAttests(attestSpecs, provenance, sbom)
			if err != nil {
				return err
			}
			if buildNamespace == "" {
				buildNamespace = opts.namespace
			}
			buildSecrets, err := parseBuildSecrets(secretSpecs)
			if err != nil {
				return err
			}
			buildSSHKeys, err := parseBuildSSH(sshSpecs)
			if err != nil {
				return err
			}
			namedContexts, err := parseNamedBuildContexts(namedContextSpecs)
			if err != nil {
				return err
			}
			defer func() {
				for _, named := range namedContexts {
					if named.context.cleanup != nil {
						named.context.cleanup()
					}
				}
			}()
			buildContext, err := prepareBuildContextInput(args[0], dockerfile, cmd.Flags().Changed("file"), os.Stdin)
			if err != nil {
				return err
			}
			if buildContext.cleanup != nil {
				defer buildContext.cleanup()
			}
			excludeBuildCredentialSources(buildContext, buildSecrets, buildSSHKeys)
			registryOptions := buildRegistryOptions{insecure: registryInsecure, authSecret: registrySecret, caSecret: registryCASecret, cachePVC: cachePVC, secrets: len(buildSecrets)+len(buildSSHKeys) > 0}
			cacheImports, err := normalizeCacheEntries(cacheFrom, registryInsecure, false)
			if err != nil {
				return err
			}
			cacheExports, err := normalizeCacheEntries(cacheTo, registryInsecure, true)
			if err != nil {
				return err
			}
			frontendOptions := buildFrontendOptions{
				buildArgs: normalizeBuildArgs(buildArgs), target: target, labels: labels,
				platforms: normalizeCommaSeparated(platforms), noCache: noCache,
				noCacheFilter: normalizeCommaSeparated(noCacheFilter), pull: pull,
				cacheFrom:     cacheImports,
				cacheTo:       cacheExports,
				secrets:       buildSecrets,
				ssh:           buildSSHKeys,
				network:       network,
				addHosts:      hosts,
				shmSize:       shmBytes,
				namedContexts: namedContexts,
				outputs:       outputs,
				attests:       attests,
				call:          callMode,
				debug:         debug,
			}
			return runBuild(cmd.Context(), opts, buildNamespace, tags, buildContext, registryOptions, frontendOptions, resultOptions, timeout, keep)
		},
	}
	cmd.Flags().StringArrayVarP(&tags, "tag", "t", nil, "image name to push (repeatable)")
	cmd.Flags().StringVarP(&dockerfile, "file", "f", "Dockerfile", "Dockerfile path relative to the build context")
	cmd.Flags().StringArrayVar(&buildArgs, "build-arg", nil, "set a build-time variable (repeatable)")
	cmd.Flags().StringVar(&target, "target", "", "set the target build stage")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "set image metadata (repeatable)")
	cmd.Flags().StringArrayVar(&platforms, "platform", nil, "set target platform (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "do not use cache when building the image")
	cmd.Flags().StringArrayVar(&noCacheFilter, "no-cache-filter", nil, "do not cache selected stages (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&pull, "pull", false, "always resolve referenced images from their registry")
	cmd.Flags().StringArrayVar(&cacheFrom, "cache-from", nil, "external cache source (repeatable)")
	cmd.Flags().StringArrayVar(&cacheTo, "cache-to", nil, "external cache destination (repeatable)")
	cmd.Flags().StringVar(&cachePVC, "cache-pvc", "", "existing PVC used for persistent BuildKit state")
	cmd.Flags().StringArrayVar(&secretSpecs, "secret", nil, "secret to expose to the build (repeatable)")
	cmd.Flags().StringArrayVar(&sshSpecs, "ssh", nil, "SSH private key to expose to the build (ID=PATH, repeatable)")
	cmd.Flags().StringVar(&networkMode, "network", "default", "network mode for RUN instructions (default or none)")
	cmd.Flags().StringArrayVar(&addHosts, "add-host", nil, "add a HOST=IP mapping to build containers (repeatable)")
	cmd.Flags().StringArrayVar(&allows, "allow", nil, "privileged entitlement (unsupported)")
	cmd.Flags().StringVar(&cgroupParent, "cgroup-parent", "", "host cgroup parent (unsupported)")
	cmd.Flags().BoolVar(&load, "load", false, "load into a Docker Engine image store (unsupported)")
	cmd.Flags().StringVar(&builder, "builder", "", "builder profile (only default is supported)")
	cmd.Flags().StringVar(&policy, "policy", "", "Buildx policy file (unsupported)")
	cmd.Flags().StringVar(&isolation, "isolation", "", "Windows isolation mode (unsupported)")
	cmd.Flags().StringArrayVar(&securityOpts, "security-opt", nil, "legacy builder security option (unsupported)")
	cmd.Flags().BoolVar(&squash, "squash", false, "legacy builder layer squashing (unsupported)")
	cmd.Flags().StringArrayVar(&resourceSpecs, "resource", nil, "builder resource setting (unsupported)")
	cmd.Flags().StringVar(&shmSize, "shm-size", "", "shared-memory size for RUN instructions")
	cmd.Flags().StringArrayVar(&ulimits, "ulimit", nil, "process limit for RUN instructions (unsupported)")
	cmd.Flags().StringArrayVar(&namedContextSpecs, "build-context", nil, "additional NAME=PATH context (repeatable)")
	cmd.Flags().StringArrayVarP(&outputSpecs, "output", "o", nil, "output destination specification (repeatable)")
	cmd.Flags().StringArrayVar(&annotations, "annotation", nil, "annotation to attach to image output (repeatable)")
	cmd.Flags().StringArrayVar(&attestSpecs, "attest", nil, "attestation parameters (type=provenance or type=sbom, repeatable)")
	cmd.Flags().StringVar(&provenance, "provenance", "", "provenance attestation settings")
	cmd.Flags().Lookup("provenance").NoOptDefVal = "true"
	cmd.Flags().StringVar(&sbom, "sbom", "", "SBOM attestation settings")
	cmd.Flags().Lookup("sbom").NoOptDefVal = "true"
	cmd.Flags().StringVar(&call, "call", "build", "Dockerfile evaluation method (build, check, outline, or targets)")
	cmd.Flags().BoolVar(&check, "check", false, "shorthand for --call=check")
	cmd.Flags().BoolVar(&debug, "debug", false, "print redacted build lifecycle diagnostics")
	cmd.Flags().StringVar(&progress, "progress", "auto", "progress output (auto, plain, quiet, or rawjson)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress build output and print the image ID")
	cmd.Flags().StringVar(&iidFile, "iidfile", "", "write the image ID to a file")
	cmd.Flags().StringVar(&metadataFile, "metadata-file", "", "write build result metadata to a file")
	cmd.Flags().StringVar(&buildNamespace, "build-namespace", "dockube-system", "namespace in which BuildKit jobs run")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "maximum build time (0 disables the deadline)")
	cmd.Flags().BoolVar(&keep, "keep-build", false, "retain the build Job after completion")
	cmd.Flags().BoolVar(&push, "push", false, "push the build result to the tagged registry")
	cmd.Flags().BoolVar(&registryInsecure, "registry-insecure", false, "allow an insecure HTTP registry connection")
	cmd.Flags().StringVar(&registrySecret, "registry-secret", "", "Kubernetes Secret containing a .dockerconfigjson registry credential")
	cmd.Flags().StringVar(&registryCASecret, "registry-ca-secret", "", "Kubernetes Secret containing the registry CA certificate in ca.crt")
	return cmd
}

func normalizeBuildResultOptions(progress string, quiet bool, iidFile, metadataFile string) (buildResultOptions, error) {
	if progress == "auto" {
		progress = "plain"
	}
	if quiet {
		if progress != "plain" && progress != "quiet" {
			return buildResultOptions{}, fmt.Errorf("--quiet conflicts with --progress=%s", progress)
		}
		progress = "quiet"
	}
	switch progress {
	case "plain", "quiet", "rawjson":
	default:
		return buildResultOptions{}, fmt.Errorf("unsupported progress mode %q; use auto, plain, quiet, or rawjson", progress)
	}
	return buildResultOptions{progress: progress, quiet: progress == "quiet", iidFile: iidFile, metadataFile: metadataFile}, nil
}

func normalizeBuildArgs(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.Contains(value, "=") {
			result = append(result, value)
			continue
		}
		result = append(result, value+"="+os.Getenv(value))
	}
	return result
}

func normalizeCommaSeparated(values []string) []string {
	var result []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				result = append(result, part)
			}
		}
	}
	return result
}

func normalizeBuildNetwork(value string) (string, error) {
	switch value {
	case "", "default":
		return "", nil
	case "none":
		return "none", nil
	case "host":
		return "", fmt.Errorf("--network=host is disabled: dockube builds cannot join the Kubernetes node network")
	default:
		return "", fmt.Errorf("unsupported network mode %q; use default or none", value)
	}
}

func normalizeBuildHosts(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		host, address, ok := strings.Cut(value, "=")
		if !ok {
			host, address, ok = strings.Cut(value, ":")
		}
		host = strings.TrimSpace(host)
		address = strings.TrimSpace(address)
		if !ok || host == "" || strings.ContainsAny(host, "\t\r\n ,=") {
			return nil, fmt.Errorf("invalid --add-host %q: expected HOST=IP", value)
		}
		if address == "host-gateway" {
			return nil, fmt.Errorf("invalid --add-host %q: host-gateway would expose the Kubernetes node network", value)
		}
		if net.ParseIP(strings.Trim(address, "[]")) == nil {
			return nil, fmt.Errorf("invalid --add-host %q: address must be an IP literal", value)
		}
		result = append(result, host+"="+address)
	}
	return result, nil
}

func normalizeShmSize(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	bytes, err := units.RAMInBytes(value)
	if err != nil || bytes <= 0 {
		return 0, fmt.Errorf("invalid --shm-size %q: expected a positive size such as 64m or 1g", value)
	}
	return bytes, nil
}

func normalizeCacheEntries(values []string, insecure, export bool) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !strings.Contains(value, "=") {
			value = "type=registry,ref=" + value
		}
		cacheType := ""
		for _, field := range strings.Split(value, ",") {
			if key, entry, ok := strings.Cut(field, "="); ok && key == "type" {
				cacheType = entry
			}
		}
		if cacheType != "registry" && !(export && cacheType == "inline") {
			return nil, fmt.Errorf("unsupported cache backend %q: dockube supports registry cache%s only; local and cloud cache credential and retention policies are undefined", cacheType, map[bool]string{true: " and inline export"}[export])
		}
		if insecure && cacheType == "registry" && !strings.Contains(value, "registry.insecure=") {
			value += ",registry.insecure=true"
		}
		result = append(result, value)
	}
	return result, nil
}

func normalizeBuildOutputs(values []string, push bool, tags []string, insecure, outputOptional bool) ([]buildOutput, error) {
	if len(values) == 0 && !push && !outputOptional {
		return nil, fmt.Errorf("dockube has no local image store; use --push or --output to select a build result")
	}
	result := make([]buildOutput, 0, len(values)+1)
	if push {
		if len(tags) == 0 {
			return nil, fmt.Errorf("an image name is required with --tag when --push is used")
		}
		spec := "type=image," + buildOutputNameAttribute(strings.Join(tags, ",")) + ",push=true"
		if insecure {
			spec += ",registry.insecure=true"
		}
		result = append(result, buildOutput{kind: "registry", spec: spec})
	}
	fileIndex := 0
	for _, value := range values {
		attributes := map[string]string{}
		for _, field := range strings.Split(value, ",") {
			key, entry, ok := strings.Cut(field, "=")
			if !ok {
				return nil, fmt.Errorf("invalid --output %q; expected type=TYPE,dest=PATH", value)
			}
			attributes[key] = entry
		}
		kind := attributes["type"]
		if kind == "registry" || kind == "image" {
			name := attributes["name"]
			if name == "" {
				name = strings.Join(tags, ",")
			}
			if name == "" {
				return nil, fmt.Errorf("registry output requires name=... or at least one --tag")
			}
			spec := "type=image," + buildOutputNameAttribute(name) + ",push=true"
			if insecure {
				spec += ",registry.insecure=true"
			}
			result = append(result, buildOutput{kind: "registry", spec: spec})
			continue
		}
		if kind != "oci" && kind != "docker" && kind != "tar" && kind != "local" {
			return nil, fmt.Errorf("unsupported output type %q; use registry, oci, docker, tar, or local", kind)
		}
		destination := attributes["dest"]
		if destination == "" {
			destination = attributes["destination"]
		}
		if destination == "" {
			return nil, fmt.Errorf("%s output requires dest=PATH", kind)
		}
		if destination == "-" {
			return nil, fmt.Errorf("dest=- is not yet supported because progress and binary output streams must remain separate; use a client file path")
		}
		podPath := fmt.Sprintf("/workspace/.dockube-output-%d", fileIndex)
		fileIndex++
		if kind != "local" {
			podPath += ".tar"
		}
		result = append(result, buildOutput{kind: kind, spec: "type=" + kind + ",dest=" + podPath, clientPath: destination, podPath: podPath})
	}
	return result, nil
}

func buildOutputNameAttribute(name string) string {
	if strings.Contains(name, ",") {
		return `"name=` + name + `"`
	}
	return "name=" + name
}

func applyBuildAnnotations(outputs []buildOutput, values []string) ([]buildOutput, error) {
	if len(values) == 0 {
		return outputs, nil
	}
	annotations := make([]string, 0, len(values))
	for _, value := range values {
		if strings.Contains(value, ",") {
			return nil, fmt.Errorf("annotation %q contains an unsupported comma", value)
		}
		location, pair, hasLocation := strings.Cut(value, ":")
		if !hasLocation {
			pair = location
			location = ""
		}
		key, annotationValue, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid annotation %q; expected [TYPE:]KEY=VALUE", value)
		}
		prefix := "annotation"
		if location != "" {
			base := strings.Split(location, "[")[0]
			switch base {
			case "manifest", "manifest-descriptor", "index", "index-descriptor":
			default:
				return nil, fmt.Errorf("invalid annotation target %q", location)
			}
			prefix += "-" + location
		}
		annotations = append(annotations, prefix+"."+key+"="+annotationValue)
	}
	applied := false
	for index := range outputs {
		if outputs[index].kind == "registry" || outputs[index].kind == "oci" || outputs[index].kind == "docker" {
			outputs[index].spec += "," + strings.Join(annotations, ",")
			applied = true
		}
	}
	if !applied {
		return nil, fmt.Errorf("--annotation requires a registry, OCI, or Docker image output")
	}
	return outputs, nil
}

func normalizeBuildAttests(values []string, provenance, sbom string) ([]string, error) {
	if provenance != "" {
		if provenance == "true" {
			provenance = "mode=min"
		} else if provenance == "false" {
			provenance = "disabled=true"
		}
		values = append(values, "type=provenance,"+provenance)
	}
	if sbom != "" {
		if sbom == "true" {
			sbom = ""
		} else if sbom == "false" {
			sbom = "disabled=true"
		}
		value := "type=sbom"
		if sbom != "" {
			value += "," + sbom
		}
		values = append(values, value)
	}
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		attributes := strings.Split(value, ",")
		if len(attributes) == 0 || !strings.HasPrefix(attributes[0], "type=") {
			return nil, fmt.Errorf("invalid attestation %q; type must be first", value)
		}
		kind := strings.TrimPrefix(attributes[0], "type=")
		if kind != "provenance" && kind != "sbom" {
			return nil, fmt.Errorf("unsupported attestation type %q; use provenance or sbom", kind)
		}
		if _, exists := seen[kind]; exists {
			return nil, fmt.Errorf("duplicate %s attestation", kind)
		}
		seen[kind] = struct{}{}
		result = append(result, "attest:"+kind+"="+strings.Join(attributes[1:], ","))
	}
	return result, nil
}

func normalizeBuildCall(value string, check bool) (string, error) {
	if check {
		if value != "build" && value != "check" {
			return "", fmt.Errorf("--check conflicts with --call=%s", value)
		}
		value = "check"
	}
	switch value {
	case "build", "check", "outline", "targets":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported build call %q; use build, check, outline, or targets", value)
	}
}

var buildSecretIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func parseNamedBuildContexts(specs []string) ([]buildNamedContext, error) {
	result := make([]buildNamedContext, 0, len(specs))
	seen := map[string]struct{}{}
	for _, spec := range specs {
		name, source, ok := strings.Cut(spec, "=")
		if !ok || !buildSecretIDPattern.MatchString(name) || source == "" {
			return nil, fmt.Errorf("invalid build context %q; expected NAME=PATH_OR_URL", spec)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate build context name %q", name)
		}
		context, err := prepareNamedBuildContext(source)
		if err != nil {
			for _, prepared := range result {
				if prepared.context.cleanup != nil {
					prepared.context.cleanup()
				}
			}
			return nil, fmt.Errorf("prepare build context %q: %w", name, err)
		}
		seen[name] = struct{}{}
		result = append(result, buildNamedContext{name: name, context: context})
	}
	return result, nil
}

func prepareNamedBuildContext(source string) (*preparedBuildContext, error) {
	var temporary []string
	cleanup := func() {
		for _, path := range temporary {
			_ = os.RemoveAll(path)
		}
	}
	root := source
	if source == "-" {
		return nil, fmt.Errorf("stdin is reserved for the primary context or Dockerfile")
	}
	if isGitContext(source) {
		path, err := cloneGitContext(source)
		if err != nil {
			return nil, err
		}
		temporary = append(temporary, path)
		root = path
	} else if isRemoteInput(source) {
		path, err := downloadRemoteInput(source, maxArchiveContextSize)
		if err != nil {
			return nil, err
		}
		temporary = append(temporary, path)
		root = path
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		cleanup()
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		cleanup()
		return nil, err
	}
	if info.Mode().IsRegular() {
		extracted, err := extractBuildArchive(absolute)
		if err != nil {
			cleanup()
			return nil, err
		}
		temporary = append(temporary, extracted)
		absolute = extracted
	} else if !info.IsDir() {
		cleanup()
		return nil, fmt.Errorf("named context is neither a directory nor a tar archive")
	}
	matcher, ignorePath, err := buildContextMatcher(absolute, ".dockube-unused-Dockerfile")
	if err != nil {
		cleanup()
		return nil, err
	}
	return &preparedBuildContext{root: absolute, dockerfile: ".dockube-unused-Dockerfile", ignorePath: ignorePath, matcher: matcher, cleanup: cleanup}, nil
}

func parseBuildSecrets(specs []string) ([]buildSecret, error) {
	result := make([]buildSecret, 0, len(specs))
	seen := map[string]struct{}{}
	for _, spec := range specs {
		attributes := map[string]string{}
		for _, field := range strings.Split(spec, ",") {
			key, value, ok := strings.Cut(field, "=")
			if !ok || key == "" {
				return nil, fmt.Errorf("invalid secret specification %q", spec)
			}
			attributes[key] = value
		}
		id := attributes["id"]
		if id == "" || !buildSecretIDPattern.MatchString(id) {
			return nil, fmt.Errorf("secret %q has an invalid or missing id", spec)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate build secret id %q", id)
		}
		secretType := attributes["type"]
		if secretType == "" {
			if _, exists := os.LookupEnv(id); exists && attributes["src"] == "" && attributes["source"] == "" {
				secretType = "env"
			} else {
				secretType = "file"
			}
		}
		var data []byte
		sourcePath := ""
		switch secretType {
		case "file":
			source := attributes["src"]
			if source == "" {
				source = attributes["source"]
			}
			if source == "" {
				source = id
			}
			var err error
			data, err = os.ReadFile(source)
			if err != nil {
				return nil, fmt.Errorf("read build secret %q: %w", id, err)
			}
			sourcePath, _ = filepath.Abs(source)
		case "env":
			source := attributes["env"]
			if source == "" {
				source = attributes["src"]
			}
			if source == "" {
				source = attributes["source"]
			}
			if source == "" {
				source = id
			}
			value, exists := os.LookupEnv(source)
			if !exists {
				return nil, fmt.Errorf("environment variable %q for build secret %q is not set", source, id)
			}
			data = []byte(value)
		default:
			return nil, fmt.Errorf("build secret %q has unsupported type %q", id, secretType)
		}
		seen[id] = struct{}{}
		result = append(result, buildSecret{id: id, data: data, source: sourcePath})
	}
	return result, nil
}

func parseBuildSSH(specs []string) ([]buildSSH, error) {
	result := make([]buildSSH, 0, len(specs))
	seen := map[string]struct{}{}
	for _, spec := range specs {
		id, source, hasSource := strings.Cut(spec, "=")
		if !hasSource {
			source = os.Getenv("SSH_AUTH_SOCK")
		}
		if id == "" {
			id = "default"
		}
		if !buildSecretIDPattern.MatchString(id) || source == "" {
			return nil, fmt.Errorf("invalid SSH specification %q; expected ID=PRIVATE_KEY_PATH", spec)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate SSH id %q", id)
		}
		info, err := os.Stat(source)
		if err != nil {
			return nil, fmt.Errorf("read SSH source %q: %w", id, err)
		}
		if info.Mode()&os.ModeSocket != 0 {
			return nil, fmt.Errorf("SSH agent sockets are not yet supported; pass %s=/path/to/private-key", id)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("SSH source %q is not a regular private-key file", id)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read SSH source %q: %w", id, err)
		}
		seen[id] = struct{}{}
		absoluteSource, _ := filepath.Abs(source)
		result = append(result, buildSSH{id: id, data: data, source: absoluteSource})
	}
	return result, nil
}

func excludeBuildCredentialSources(context *preparedBuildContext, secrets []buildSecret, sshKeys []buildSSH) {
	context.excludedPaths = map[string]struct{}{}
	add := func(source string) {
		if source == "" {
			return
		}
		relative, err := filepath.Rel(context.root, source)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			context.excludedPaths[filepath.ToSlash(relative)] = struct{}{}
		}
	}
	for _, secret := range secrets {
		add(secret.source)
	}
	for _, ssh := range sshKeys {
		add(ssh.source)
	}
}

func archiveBuildContext(root, dockerfile string) ([]byte, error) {
	buildContext, err := prepareBuildContext(root, dockerfile)
	if err != nil {
		return nil, err
	}
	var compressed bytes.Buffer
	if err := writeBuildContext(buildContext, &compressed); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func prepareBuildContext(root, dockerfile string) (*preparedBuildContext, error) {
	return prepareBuildContextWithFile(root, dockerfile, false)
}

func prepareBuildContextInput(root, dockerfile string, explicitFile bool, input io.Reader) (*preparedBuildContext, error) {
	if root == "-" && dockerfile == "-" {
		return nil, fmt.Errorf("stdin cannot provide both the build context and Dockerfile")
	}
	var temporary []string
	cleanupTemporary := func() {
		for _, path := range temporary {
			_ = os.RemoveAll(path)
		}
	}
	if isRemoteInput(dockerfile) {
		path, err := downloadRemoteInput(dockerfile, 16<<20)
		if err != nil {
			return nil, fmt.Errorf("download remote Dockerfile: %w", err)
		}
		temporary = append(temporary, path)
		dockerfile = path
		explicitFile = true
	}
	if isGitContext(root) {
		path, err := cloneGitContext(root)
		if err != nil {
			cleanupTemporary()
			return nil, err
		}
		temporary = append(temporary, path)
		root = path
	} else if isRemoteInput(root) {
		path, err := downloadRemoteInput(root, maxArchiveContextSize)
		if err != nil {
			cleanupTemporary()
			return nil, fmt.Errorf("download remote build context: %w", err)
		}
		temporary = append(temporary, path)
		root = path
	}
	if root == "-" {
		file, err := os.CreateTemp("", "dockube-stdin-context-*.tar")
		if err != nil {
			return nil, err
		}
		temporary = append(temporary, file.Name())
		written, copyErr := io.Copy(file, io.LimitReader(input, maxArchiveContextSize+1))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			cleanupTemporary()
			if copyErr != nil {
				return nil, copyErr
			}
			return nil, closeErr
		}
		if written > maxArchiveContextSize {
			cleanupTemporary()
			return nil, fmt.Errorf("stdin build context exceeds the 2 GiB limit")
		}
		root = file.Name()
	}
	if dockerfile == "-" {
		file, err := os.CreateTemp("", "dockube-stdin-Dockerfile-")
		if err != nil {
			cleanupTemporary()
			return nil, err
		}
		temporary = append(temporary, file.Name())
		written, copyErr := io.Copy(file, io.LimitReader(input, 16<<20+1))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || written > 16<<20 {
			cleanupTemporary()
			if written > 16<<20 {
				return nil, fmt.Errorf("stdin Dockerfile exceeds the 16 MiB limit")
			}
			if copyErr != nil {
				return nil, copyErr
			}
			return nil, closeErr
		}
		dockerfile = file.Name()
		explicitFile = true
	}
	context, err := prepareBuildContextWithFile(root, dockerfile, explicitFile)
	if err != nil {
		if isTextBuildContext(root, err) && dockerfile == "Dockerfile" {
			directory, textErr := textBuildContext(root)
			if textErr == nil {
				temporary = append(temporary, directory)
				context, err = prepareBuildContextWithFile(directory, "Dockerfile", false)
			}
		}
	}
	if err != nil {
		cleanupTemporary()
		return nil, err
	}
	priorCleanup := context.cleanup
	context.cleanup = func() {
		if priorCleanup != nil {
			priorCleanup()
		}
		cleanupTemporary()
	}
	return context, nil
}

func isRemoteInput(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http")
}

func isGitContext(value string) bool {
	return strings.HasPrefix(value, "git+https://") || strings.HasPrefix(value, "file://") || (strings.HasPrefix(value, "https://") && strings.Contains(strings.Split(value, "#")[0], ".git"))
}

func validateRemoteURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("URL-embedded credentials are not allowed; use an administrator-provided credential mechanism")
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("remote input scheme %q is not allowed; use verified HTTPS", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("remote input URL has no host")
	}
	return parsed, nil
}

func downloadRemoteInput(value string, limit int64) (string, error) {
	parsed, err := validateRemoteURL(value)
	if err != nil {
		return "", err
	}
	redirects := 0
	client := &http.Client{
		Transport: remoteHTTPTransport,
		Timeout:   10 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			redirects++
			if redirects > 5 {
				return fmt.Errorf("remote input exceeded five redirects")
			}
			if _, err := validateRemoteURL(request.URL.String()); err != nil {
				return fmt.Errorf("unsafe remote redirect: %w", err)
			}
			return nil
		},
	}
	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("remote input returned HTTP %s", response.Status)
	}
	if response.ContentLength > limit {
		return "", fmt.Errorf("remote input exceeds the %d-byte limit", limit)
	}
	file, err := os.CreateTemp("", "dockube-remote-input-")
	if err != nil {
		return "", err
	}
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = os.Remove(file.Name())
		}
	}()
	written, err := io.Copy(file, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", err
	}
	if written > limit {
		return "", fmt.Errorf("remote input exceeds the %d-byte limit", limit)
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	failed = false
	return file.Name(), nil
}

func cloneGitContext(value string) (string, error) {
	cloneURL := strings.TrimPrefix(value, "git+")
	ref := ""
	if base, fragment, ok := strings.Cut(cloneURL, "#"); ok {
		cloneURL, ref = base, fragment
	}
	parsed, err := url.Parse(cloneURL)
	if err != nil || parsed.User != nil {
		return "", fmt.Errorf("invalid Git context URL or embedded credentials")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "file" {
		return "", fmt.Errorf("Git context scheme %q is unsupported; use verified HTTPS", parsed.Scheme)
	}
	root, err := os.MkdirTemp("", "dockube-git-context-")
	if err != nil {
		return "", err
	}
	args := []string{"clone", "--depth=1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, "--", cloneURL, root)
	command := exec.Command("git", args...)
	if output, err := command.CombinedOutput(); err != nil {
		_ = os.RemoveAll(root)
		return "", fmt.Errorf("clone Git build context: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var total int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			if total > maxArchiveContextSize {
				return fmt.Errorf("Git build context exceeds the 2 GiB limit")
			}
		}
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(root)
		return "", err
	}
	return root, nil
}

func isTextBuildContext(path string, prepareErr error) bool {
	return strings.Contains(prepareErr.Error(), "read build context archive") && strings.Contains(filepath.Base(path), "dockube-remote-input-")
}

func textBuildContext(source string) (string, error) {
	data, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	if len(data) > 16<<20 || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("remote text Dockerfile is not valid UTF-8 or exceeds 16 MiB")
	}
	root, err := os.MkdirTemp("", "dockube-text-context-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), data, 0o600); err != nil {
		_ = os.RemoveAll(root)
		return "", err
	}
	return root, nil
}

func prepareBuildContextWithFile(root, dockerfile string, explicitFile bool) (*preparedBuildContext, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	var cleanup func()
	if info.Mode().IsRegular() {
		extracted, err := extractBuildArchive(root)
		if err != nil {
			return nil, err
		}
		root = extracted
		cleanup = func() { _ = os.RemoveAll(extracted) }
	} else if !info.IsDir() {
		return nil, fmt.Errorf("build context %q is neither a directory nor a tar archive", root)
	}
	if dockerfile == "-" {
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("Dockerfile from stdin is not yet supported")
	}
	dockerfile = filepath.Clean(dockerfile)
	if !explicitFile && (filepath.IsAbs(dockerfile) || dockerfile == ".." || strings.HasPrefix(dockerfile, ".."+string(filepath.Separator))) {
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("Dockerfile must be inside the build context unless -f/--file is set explicitly")
	}
	dockerfileSource := filepath.Join(root, dockerfile)
	if explicitFile {
		dockerfileSource, err = filepath.Abs(dockerfile)
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			return nil, err
		}
	}
	if filepath.IsAbs(dockerfile) {
		dockerfileSource = dockerfile
	}
	if info, statErr := os.Stat(dockerfileSource); statErr != nil || info.IsDir() {
		if cleanup != nil {
			cleanup()
		}
		err = statErr
		if err == nil {
			err = fmt.Errorf("is a directory")
		}
		return nil, fmt.Errorf("Dockerfile %q: %w", dockerfile, err)
	}
	if info.Mode().Perm()&0o444 == 0 {
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("Dockerfile %q is not readable", dockerfile)
	}
	dockerfileTarget := ".dockube.Dockerfile"
	if relative, relErr := filepath.Rel(root, dockerfileSource); relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		dockerfileTarget = filepath.ToSlash(relative)
		dockerfileSource = ""
	}
	matcher, ignorePath, err := buildContextMatcher(root, dockerfileTarget)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}
	return &preparedBuildContext{root: root, dockerfile: dockerfileTarget, ignorePath: ignorePath, matcher: matcher, dockerfileSource: dockerfileSource, cleanup: cleanup}, nil
}

const maxArchiveContextSize = int64(2 << 30)

var remoteHTTPTransport http.RoundTripper = http.DefaultTransport

func extractBuildArchive(source string) (string, error) {
	file, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var reader io.Reader = file
	if gz, gzipErr := gzip.NewReader(file); gzipErr == nil {
		defer gz.Close()
		reader = gz
	} else if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	root, err := os.MkdirTemp("", "dockube-context-")
	if err != nil {
		return "", err
	}
	failed := true
	defer func() {
		if failed {
			_ = os.RemoveAll(root)
		}
	}()
	tr := tar.NewReader(reader)
	var total int64
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read build context archive: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe path %q in build context archive", header.Name)
		}
		target := filepath.Join(root, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg, tar.TypeRegA:
			total += header.Size
			if header.Size < 0 || total > maxArchiveContextSize {
				return "", fmt.Errorf("build context archive exceeds the 2 GiB extraction limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return "", err
			}
			_, copyErr := io.CopyN(output, tr, header.Size)
			closeErr := output.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("unsupported special file %q in build context archive", header.Name)
		}
	}
	failed = false
	return root, nil
}

func writeBuildContext(buildContext *preparedBuildContext, destination io.Writer) error {
	gz := gzip.NewWriter(destination)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(buildContext.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(buildContext.root, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, excluded := buildContext.excludedPaths[rel]; excluded {
			return nil
		}
		ignored, err := buildContext.matcher.MatchesOrParentMatches(rel)
		if err != nil {
			return fmt.Errorf("match build context path %q: %w", rel, err)
		}
		// Dockerfiles and the selected ignore file must reach the frontend even
		// when an ignore rule matches them. BuildKit prevents COPY from using
		// these files separately.
		if ignored && rel != buildContext.dockerfile && rel != buildContext.ignorePath {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if fileInfo.Mode().IsRegular() && fileInfo.Mode().Perm()&0o444 == 0 {
			return fmt.Errorf("build context file %q is not readable", rel)
		}
		if fileInfo.Mode()&os.ModeType != 0 && !fileInfo.IsDir() && fileInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported special file %q in build context", rel)
		}
		link := ""
		if fileInfo.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(fileInfo, link)
		if err != nil {
			return err
		}
		header.Name = rel
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err == nil && buildContext.dockerfileSource != "" {
		file, openErr := os.Open(buildContext.dockerfileSource)
		if openErr != nil {
			err = openErr
		} else {
			info, statErr := file.Stat()
			if statErr == nil {
				var header *tar.Header
				header, statErr = tar.FileInfoHeader(info, "")
				if statErr == nil {
					header.Name = buildContext.dockerfile
					statErr = tw.WriteHeader(header)
				}
				if statErr == nil {
					_, statErr = io.Copy(tw, file)
				}
			}
			closeErr := file.Close()
			if statErr != nil {
				err = statErr
			} else if closeErr != nil {
				err = closeErr
			}
		}
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err != nil {
		return err
	}
	return nil
}

func buildContextMatcher(root, dockerfile string) (*patternmatcher.PatternMatcher, string, error) {
	ignorePath := filepath.ToSlash(dockerfile + ".dockerignore")
	file, err := os.Open(filepath.Join(root, filepath.FromSlash(ignorePath)))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, "", fmt.Errorf("open Dockerfile-specific ignore file: %w", err)
		}
		ignorePath = ".dockerignore"
		file, err = os.Open(filepath.Join(root, ignorePath))
	}
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, "", fmt.Errorf("open .dockerignore: %w", err)
		}
		matcher, matchErr := patternmatcher.New(nil)
		return matcher, "", matchErr
	}
	defer file.Close()

	patterns, err := ignorefile.ReadAll(file)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", ignorePath, err)
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", ignorePath, err)
	}
	return matcher, ignorePath, nil
}

func runBuild(ctx context.Context, opts *options, namespace string, images []string, buildContext *preparedBuildContext, registryOptions buildRegistryOptions, frontendOptions buildFrontendOptions, resultOptions buildResultOptions, timeout time.Duration, keep bool) error {
	name := "dockube-build-" + strings.ToLower(rand.String(8))
	if frontendOptions.debug {
		fmt.Fprintln(opts.errOut, buildDebugMessage(name, namespace, frontendOptions, keep, timeout))
	}
	jobs := opts.core.BatchV1().Jobs(namespace)
	buildEnv, buildMounts, buildVolumes := buildRegistryConfiguration(images, buildContext.dockerfile, registryOptions)
	needsMetadata := resultOptions.quiet || resultOptions.iidFile != "" || resultOptions.metadataFile != ""
	collectResult := needsMetadata
	for _, output := range frontendOptions.outputs {
		collectResult = collectResult || output.clientPath != ""
	}
	frontendArguments := buildFrontendArguments(frontendOptions)
	progress := resultOptions.progress
	if progress == "quiet" {
		progress = "plain"
	}
	frontendArguments = append(frontendArguments, "--progress="+progress)
	if needsMetadata {
		frontendArguments = append(frontendArguments, "--metadata-file", "/workspace/.dockube-metadata.json")
	}
	buildArgs := append([]string{buildCommandScript(collectResult), "--"}, frontendArguments...)

	backoff := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "dockube"}},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "dockube", "dockube.io/build": name}},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolptr(false),
					SecurityContext:              &corev1.PodSecurityContext{FSGroup: int64ptr(1000)},
					Containers: []corev1.Container{{
						Name:         "buildkit",
						Image:        buildkitImage,
						Command:      []string{"/bin/sh", "-ec"},
						Args:         buildArgs,
						Env:          buildEnv,
						VolumeMounts: buildMounts,
						Resources:    buildResources(),
						// RootlessKit requires unconfined syscall/AppArmor policy and its setuid
						// newuidmap helper. The main process remains UID 1000 and receives no
						// host mounts or Kubernetes service-account token.
						SecurityContext: &corev1.SecurityContext{RunAsUser: int64ptr(1000), RunAsGroup: int64ptr(1000), AllowPrivilegeEscalation: boolptr(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}, AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}},
					}},
					Volumes: buildVolumes,
				},
			},
		},
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create BuildKit job: %w", err)
	}
	if !keep {
		defer func() {
			policy := metav1.DeletePropagationBackground
			_ = jobs.Delete(context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &policy})
		}()
	}

	if !resultOptions.quiet && resultOptions.progress != "rawjson" {
		fmt.Fprintf(opts.out, "Building %s with job %s/%s\n", strings.Join(images, ", "), namespace, name)
	}
	waitCtx, cancel := buildWaitContext(ctx, timeout)
	defer cancel()
	podName, err := waitForBuildPod(waitCtx, opts, namespace, name)
	if err == nil {
		err = uploadBuildContext(waitCtx, opts, namespace, podName, buildContext)
	}
	if err == nil {
		for _, named := range frontendOptions.namedContexts {
			if err = uploadBuildContextAt(waitCtx, opts, namespace, podName, named.context, "/home/user/.dockube-contexts/"+named.name); err != nil {
				break
			}
		}
	}
	if err == nil && len(frontendOptions.secrets)+len(frontendOptions.ssh) > 0 {
		err = uploadBuildSecrets(waitCtx, opts, namespace, podName, frontendOptions.secrets, frontendOptions.ssh)
	}
	if err == nil {
		err = signalBuildReady(waitCtx, opts, namespace, podName)
	}
	var logResult chan error
	if err == nil {
		logResult = make(chan error, 1)
		go func() {
			logResult <- streamBuildLogs(waitCtx, opts, namespace, podName, resultOptions.quiet)
		}()
	}
	var metadata []byte
	if err == nil && collectResult {
		err = waitForBuildResult(waitCtx, opts, namespace, podName)
		if err == nil && needsMetadata {
			metadata, err = readBuildResult(waitCtx, opts, namespace, podName)
		}
		if err == nil {
			for _, output := range frontendOptions.outputs {
				if output.clientPath != "" {
					if err = collectBuildOutput(waitCtx, opts, namespace, podName, output); err != nil {
						break
					}
				}
			}
		}
		signalErr := signalBuildResultCollected(waitCtx, opts, namespace, podName)
		if err == nil {
			err = signalErr
		}
	}
	if err == nil {
		err = waitForBuild(waitCtx, opts, namespace, name)
	}
	var logErr error
	if logResult != nil {
		logErr = <-logResult
	}
	if terminalErr := finalBuildError(err, logErr); terminalErr != nil {
		return terminalErr
	}
	if resultOptions.metadataFile != "" {
		if err := os.WriteFile(resultOptions.metadataFile, metadata, 0o644); err != nil {
			return fmt.Errorf("write metadata file: %w", err)
		}
	}
	if resultOptions.iidFile != "" || resultOptions.quiet {
		imageID, err := buildImageID(metadata)
		if err != nil {
			return err
		}
		if resultOptions.iidFile != "" {
			if err := os.WriteFile(resultOptions.iidFile, []byte(imageID+"\n"), 0o644); err != nil {
				return fmt.Errorf("write image ID file: %w", err)
			}
		}
		if resultOptions.quiet {
			fmt.Fprintln(opts.out, imageID)
		}
	}
	if !resultOptions.quiet && resultOptions.progress != "rawjson" {
		if len(images) > 0 {
			fmt.Fprintf(opts.out, "Successfully completed build for %s\n", strings.Join(images, ", "))
		} else {
			fmt.Fprintln(opts.out, "Successfully wrote build output")
		}
	}
	return nil
}

func finalBuildError(buildErr, logErr error) error {
	if buildErr != nil {
		return buildErr
	}
	return logErr
}

func buildDebugMessage(name, namespace string, options buildFrontendOptions, keep bool, timeout time.Duration) string {
	return fmt.Sprintf("dockube build debug: job=%s namespace=%s buildkit=%s outputs=%d named-contexts=%d secrets=%d ssh=%d keep=%t timeout-enabled=%t",
		name, namespace, buildkitImage, len(options.outputs), len(options.namedContexts), len(options.secrets), len(options.ssh), keep, timeout > 0)
}

func buildCommandScript(collectResult bool) string {
	prefix := `while [ ! -f /workspace/.dockube-context-ready ]; do sleep 0.1; done
rm /workspace/.dockube-context-ready`
	command := `buildctl-daemonless.sh build \
  --frontend dockerfile.v0 \
  --local context=/workspace \
  --local dockerfile=/workspace \
  --opt filename="$DOCKERFILE" \
  "$@"`
	if !collectResult {
		return prefix + "\nexec " + command
	}
	return prefix + `
set +e
` + command + `
status=$?
touch /workspace/.dockube-build-complete
while [ ! -f /workspace/.dockube-result-collected ]; do sleep 0.1; done
exit "$status"`
}

func buildImageID(metadata []byte) (string, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &values); err != nil {
		return "", fmt.Errorf("parse build metadata: %w", err)
	}
	var imageID string
	if value, exists := values["containerimage.config.digest"]; exists {
		if err := json.Unmarshal(value, &imageID); err != nil {
			return "", fmt.Errorf("parse image ID: %w", err)
		}
	}
	if imageID == "" {
		return "", fmt.Errorf("build metadata did not contain an image ID")
	}
	return imageID, nil
}

func buildWaitContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func buildRegistryConfiguration(images []string, dockerfile string, options buildRegistryOptions) ([]corev1.EnvVar, []corev1.VolumeMount, []corev1.Volume) {
	_ = images
	env := []corev1.EnvVar{
		{Name: "DOCKERFILE", Value: dockerfile},
		{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
	}
	mounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "buildkit", MountPath: "/home/user/.local/share/buildkit"},
	}
	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if options.cachePVC == "" {
		volumes = append(volumes, corev1.Volume{Name: "buildkit", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	} else {
		volumes = append(volumes, corev1.Volume{Name: "buildkit", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: options.cachePVC}}})
	}
	if options.authSecret != "" {
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/run/dockube/docker"})
		mounts = append(mounts, corev1.VolumeMount{Name: "registry-auth", MountPath: "/run/dockube/docker", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "registry-auth",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: options.authSecret,
				Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
			}},
		})
	}
	if options.caSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "registry-ca", MountPath: "/etc/ssl/certs/dockube-registry-ca.crt", SubPath: "ca.crt", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "registry-ca",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: options.caSecret,
				Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
			}},
		})
	}
	if options.secrets {
		mounts = append(mounts, corev1.VolumeMount{Name: "build-secrets", MountPath: "/run/dockube/secrets"})
		volumes = append(volumes, corev1.Volume{Name: "build-secrets", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}})
	}
	return env, mounts, volumes
}

func buildFrontendArguments(options buildFrontendOptions) []string {
	var result []string
	for _, buildArg := range options.buildArgs {
		result = append(result, "--opt", "build-arg:"+buildArg)
	}
	if options.target != "" {
		result = append(result, "--opt", "target="+options.target)
	}
	for _, label := range options.labels {
		result = append(result, "--opt", "label:"+label)
	}
	if len(options.platforms) > 0 {
		result = append(result, "--opt", "platform="+strings.Join(options.platforms, ","))
	}
	if options.noCache {
		result = append(result, "--no-cache")
	}
	if len(options.noCacheFilter) > 0 {
		result = append(result, "--opt", "no-cache="+strings.Join(options.noCacheFilter, ","))
	}
	if options.pull {
		result = append(result, "--opt", "image-resolve-mode=pull")
	}
	if options.network != "" {
		result = append(result, "--opt", "force-network-mode="+options.network)
	}
	if len(options.addHosts) > 0 {
		result = append(result, "--opt", "add-hosts="+strings.Join(options.addHosts, ","))
	}
	if options.shmSize > 0 {
		result = append(result, "--opt", fmt.Sprintf("shm-size=%d", options.shmSize))
	}
	for _, named := range options.namedContexts {
		result = append(result,
			"--local", named.name+"=/home/user/.dockube-contexts/"+named.name,
			"--opt", "context:"+named.name+"=local:"+named.name,
		)
	}
	for _, output := range options.outputs {
		result = append(result, "--output", output.spec)
	}
	for _, attest := range options.attests {
		result = append(result, "--opt", attest)
	}
	if options.call != "" && options.call != "build" {
		result = append(result, "--opt", "call="+options.call)
	}
	for _, cache := range options.cacheFrom {
		result = append(result, "--import-cache", cache)
	}
	for _, cache := range options.cacheTo {
		result = append(result, "--export-cache", cache)
	}
	for _, secret := range options.secrets {
		result = append(result, "--secret", "id="+secret.id+",src=/run/dockube/secrets/"+secret.id)
	}
	for _, ssh := range options.ssh {
		result = append(result, "--ssh", ssh.id+"=/run/dockube/secrets/ssh-"+ssh.id)
	}
	return result
}

func buildResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("250m"),
			corev1.ResourceMemory:           resource.MustParse("256Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("2"),
			corev1.ResourceMemory:           resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
		},
	}
}

func waitForBuildPod(ctx context.Context, opts *options, namespace, name string) (string, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		pods, err := opts.core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
		if err != nil {
			return "", err
		}
		if len(pods.Items) > 0 {
			pod := pods.Items[0]
			if startupErr := buildPodStartupError(&pod); startupErr != nil {
				return "", startupErr
			}
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return pod.Name, nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return "", fmt.Errorf("BuildKit Pod terminated before context upload: %s", pod.Status.Message)
			}
			if warning, err := buildWarningEvent(ctx, opts, namespace, "Pod", pod.Name); err != nil {
				return "", err
			} else if warning != "" {
				return "", fmt.Errorf("BuildKit Pod startup failed: %s", warning)
			}
		}
		if warning, err := buildWarningEvent(ctx, opts, namespace, "Job", name); err != nil {
			return "", err
		} else if warning != "" {
			return "", fmt.Errorf("BuildKit Job could not create a Pod: %s", warning)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for BuildKit Pod: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func buildPodStartupError(pod *corev1.Pod) error {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
			return fmt.Errorf("BuildKit Pod is unschedulable: %s", condition.Message)
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting == nil {
			continue
		}
		switch status.State.Waiting.Reason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
			return fmt.Errorf("BuildKit Pod startup failed (%s): %s", status.State.Waiting.Reason, status.State.Waiting.Message)
		}
	}
	return nil
}

func buildWarningEvent(ctx context.Context, opts *options, namespace, kind, name string) (string, error) {
	events, err := opts.core.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=" + kind + ",involvedObject.name=" + name + ",type=Warning",
	})
	if err != nil {
		return "", err
	}
	if len(events.Items) == 0 {
		return "", nil
	}
	event := events.Items[len(events.Items)-1]
	if event.Reason == "" {
		return event.Message, nil
	}
	return event.Reason + ": " + event.Message, nil
}

func uploadBuildContext(ctx context.Context, opts *options, namespace, podName string, buildContext *preparedBuildContext) error {
	return uploadBuildContextAt(ctx, opts, namespace, podName, buildContext, "/workspace")
}

func uploadBuildContextAt(ctx context.Context, opts *options, namespace, podName string, buildContext *preparedBuildContext, destination string) error {
	request := opts.core.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "buildkit",
			Command:   []string{"/bin/sh", "-ec", `mkdir -p "$1"; tar -xzf - -C "$1"`, "dockube-upload", destination},
			Stdin:     true,
			Stdout:    false,
			Stderr:    true,
		}, clientscheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(opts.config, "POST", request.URL())
	if err != nil {
		return fmt.Errorf("create build context uploader: %w", err)
	}

	reader, writer := io.Pipe()
	archiveResult := make(chan error, 1)
	go func() {
		writeErr := writeBuildContext(buildContext, writer)
		closeErr := writer.CloseWithError(writeErr)
		if writeErr != nil {
			archiveResult <- writeErr
			return
		}
		archiveResult <- closeErr
	}()
	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: reader, Stderr: opts.errOut})
	_ = reader.Close()
	archiveErr := <-archiveResult
	if archiveErr != nil {
		return fmt.Errorf("stream build context: %w", archiveErr)
	}
	if streamErr != nil {
		return fmt.Errorf("upload build context: %w", streamErr)
	}
	return nil
}

func uploadBuildSecrets(ctx context.Context, opts *options, namespace, podName string, secrets []buildSecret, sshKeys []buildSSH) error {
	request := opts.core.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "buildkit",
			Command:   []string{"/bin/sh", "-ec", "tar -xf - -C /run/dockube/secrets"},
			Stdin:     true,
			Stdout:    false,
			Stderr:    true,
		}, clientscheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(opts.config, "POST", request.URL())
	if err != nil {
		return fmt.Errorf("create build secret uploader: %w", err)
	}

	reader, writer := io.Pipe()
	archiveResult := make(chan error, 1)
	go func() {
		tarWriter := tar.NewWriter(writer)
		var writeErr error
		files := make([]buildSecret, 0, len(secrets)+len(sshKeys))
		files = append(files, secrets...)
		for _, ssh := range sshKeys {
			files = append(files, buildSecret{id: "ssh-" + ssh.id, data: ssh.data})
		}
		for _, secret := range files {
			header := &tar.Header{Name: secret.id, Mode: 0o400, Size: int64(len(secret.data)), Typeflag: tar.TypeReg}
			if writeErr = tarWriter.WriteHeader(header); writeErr != nil {
				break
			}
			if _, writeErr = tarWriter.Write(secret.data); writeErr != nil {
				break
			}
		}
		if writeErr == nil {
			writeErr = tarWriter.Close()
		}
		closeErr := writer.CloseWithError(writeErr)
		if writeErr != nil {
			archiveResult <- writeErr
			return
		}
		archiveResult <- closeErr
	}()
	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: reader, Stderr: opts.errOut})
	_ = reader.Close()
	archiveErr := <-archiveResult
	if archiveErr != nil {
		return fmt.Errorf("stream build secrets: %w", archiveErr)
	}
	if streamErr != nil {
		return fmt.Errorf("upload build secrets: %w", streamErr)
	}
	return nil
}

func signalBuildReady(ctx context.Context, opts *options, namespace, podName string) error {
	request := opts.core.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "buildkit",
			Command:   []string{"/bin/sh", "-ec", "touch /workspace/.dockube-context-ready"},
			Stderr:    true,
		}, clientscheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(opts.config, "POST", request.URL())
	if err != nil {
		return fmt.Errorf("create BuildKit start signal: %w", err)
	}
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stderr: opts.errOut}); err != nil {
		return fmt.Errorf("signal BuildKit context readiness: %w", err)
	}
	return nil
}

func waitForBuildResult(ctx context.Context, opts *options, namespace, podName string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var stderr bytes.Buffer
		err := runBuildPodCommand(ctx, opts, namespace, podName, []string{"/bin/sh", "-c", "test -f /workspace/.dockube-build-complete"}, io.Discard, &stderr)
		if err == nil {
			return nil
		}
		pod, getErr := opts.core.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("BuildKit Pod terminated before publishing result metadata: %s", pod.Status.Message)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for BuildKit result: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func readBuildResult(ctx context.Context, opts *options, namespace, podName string) ([]byte, error) {
	var output bytes.Buffer
	var stderr bytes.Buffer
	if err := runBuildPodCommand(ctx, opts, namespace, podName, []string{"/bin/cat", "/workspace/.dockube-metadata.json"}, &output, &stderr); err != nil {
		return nil, fmt.Errorf("read build metadata: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return output.Bytes(), nil
}

func signalBuildResultCollected(ctx context.Context, opts *options, namespace, podName string) error {
	var stderr bytes.Buffer
	if err := runBuildPodCommand(ctx, opts, namespace, podName, []string{"/bin/sh", "-c", "touch /workspace/.dockube-result-collected"}, io.Discard, &stderr); err != nil {
		return fmt.Errorf("release BuildKit result: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func collectBuildOutput(ctx context.Context, opts *options, namespace, podName string, output buildOutput) error {
	destination, err := filepath.Abs(output.clientPath)
	if err != nil {
		return err
	}
	if output.kind == "local" {
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("local output destination %q already exists", destination)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		temporary, err := os.MkdirTemp(filepath.Dir(destination), ".dockube-output-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temporary)
		reader, writer := io.Pipe()
		extractResult := make(chan error, 1)
		go func() {
			extractResult <- extractLocalOutput(reader, temporary)
		}()
		var stderr bytes.Buffer
		runErr := runBuildPodCommand(ctx, opts, namespace, podName, []string{"/bin/tar", "-czf", "-", "-C", output.podPath, "."}, writer, &stderr)
		_ = writer.CloseWithError(runErr)
		extractErr := <-extractResult
		if runErr != nil {
			return fmt.Errorf("stream local output: %w: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		if extractErr != nil {
			return extractErr
		}
		if err := os.Rename(temporary, destination); err != nil {
			return fmt.Errorf("install local output: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".dockube-output-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	var stderr bytes.Buffer
	runErr := runBuildPodCommand(ctx, opts, namespace, podName, []string{"/bin/cat", output.podPath}, file, &stderr)
	closeErr := file.Close()
	if runErr != nil {
		return fmt.Errorf("stream %s output: %w: %s", output.kind, runErr, strings.TrimSpace(stderr.String()))
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("install %s output: %w", output.kind, err)
	}
	return nil
}

func extractLocalOutput(source io.Reader, destination string) error {
	gz, err := gzip.NewReader(source)
	if err != nil {
		return fmt.Errorf("read local output stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path %q in local output", header.Name)
		}
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tr, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsafe special file %q in local output", header.Name)
		}
	}
}

func runBuildPodCommand(ctx context.Context, opts *options, namespace, podName string, command []string, stdout, stderr io.Writer) error {
	request := opts.core.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "buildkit",
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, clientscheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(opts.config, "POST", request.URL())
	if err != nil {
		return err
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
}

func waitForBuild(ctx context.Context, opts *options, namespace, name string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := opts.core.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, condition := range job.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			switch condition.Type {
			case batchv1.JobComplete:
				return nil
			case batchv1.JobFailed:
				return fmt.Errorf("BuildKit job failed: %s", condition.Message)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for BuildKit job: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func streamBuildLogs(ctx context.Context, opts *options, namespace, podName string, quiet bool) error {
	stream, err := opts.core.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "buildkit", Follow: true}).Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	destination := opts.out
	if quiet {
		destination = io.Discard
	}
	_, err = io.Copy(destination, stream)
	return err
}

func int64ptr(value int64) *int64 { return &value }
func boolptr(value bool) *bool    { return &value }
