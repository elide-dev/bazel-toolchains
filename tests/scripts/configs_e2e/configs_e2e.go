// Copyright 2021 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Binary configs_e2e runs an end to end test on cmd/rbe_configs_gen/rbe_configs_gen.go &
// cmd/rbe_configs_upload/rbe_configs_upload.go by:
// 1. Take the URL to the toolchain configs tarball & manifest JSON generated by rbe_configs_gen and
//    uploaded by rbe_configs_upload to GCS.
// 2. Copying the example C++ & Java hello world examples from
//    //examples/remotebuildexecution/hello_world in github.com/bazelbuild/bazel-toolchains and
//    generating Bazel WORKSPACE & .bazelrc files configured to run a remote build using the
//    toolchain configs available at the URLs from (1). This tool accepts a path to the
//    root directory of the bazel-toolchains repo cloned locally.
// 3. This tool also takes the path to an output directory where the test repository will be
//    created and a Bazel remote build will be run. Existing contents of this directory will be
//    deleted before the test files are created.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"text/template"
	"time"

	"github.com/bazelbuild/bazel-toolchains/pkg/rbeconfigsgen"
)

var (
	manifestURL    = flag.String("manifest_url", "", "Public URL to the JSON manifest uploaded to GCS by rbe_configs_upload.")
	configsURL     = flag.String("configs_url", "", "Public URL to the configs tarball uploaded to GCS by rbe_configs_upload.")
	srcRoot        = flag.String("src_root", "", "Path to root directory of the bazel-toolchains Github repo.")
	destRoot       = flag.String("dest_root", "", "Path to an empty or non-existent output directory where the Bazel Hello world repo will be set up & a Bazel build will be executed.")
	rbeInstance    = flag.String("rbe_instance", "", "Name of the RBE instance to test the configs on in the format projects/<GCP project ID>/instances/<RBE Instance ID>.")
	timeoutSeconds = flag.Int("timeout_seconds", 0, "Number of seconds before the Bazel build run in the test is killed and a timeout failure is declared.")

	// filesToCopy are the files that'll be copied from srcRoot to destRoot.
	filesToCopy = []string{
		// C++ Hello World example.
		"examples/remotebuildexecution/hello_world/cc/BUILD",
		"examples/remotebuildexecution/hello_world/cc/hello_world.cc",
		"examples/remotebuildexecution/hello_world/cc/say_hello_test.cc",
		"examples/remotebuildexecution/hello_world/cc/say_hello.cc",
		"examples/remotebuildexecution/hello_world/cc/say_hello.h",

		// Java Hello World example.
		"examples/remotebuildexecution/hello_world/java/BUILD",
		"examples/remotebuildexecution/hello_world/java/HelloWorld.java",
	}

	// workspaceTemplate is the template to create the Bazel WORKSPACE file in the test repo.
	workspaceTemplate = template.Must(template.New("WORKSPACE").Parse(`
load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

http_archive(
    name = "rbe_default",
    urls = ["{{ .ConfigsTarballURL }}"],
    sha256 = "{{ .ConfigsTarballDigest }}",
)

`))
)

// downloadManifest downloads the JSON manifest generated by rbeconfigsgen from the given URL. We
// ignore any fields added by rbe_configs_upload when it uploaded the manifest to GCS because they
// don't serve any functional purpose.
func downloadManifest(u string) (*rbeconfigsgen.Manifest, error) {
	resp, err := http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("unable to create a HTTP GET request to download the config manifest from %q: %w", u, err)
	}
	defer resp.Body.Close()

	result := &rbeconfigsgen.Manifest{}
	um := json.NewDecoder(resp.Body)
	if err := um.Decode(result); err != nil {
		return nil, fmt.Errorf("failed to download/parse the manifest from %q: %w", u, err)
	}
	if len(result.BazelVersion) == 0 {
		return nil, fmt.Errorf("manifest downloaded from %q did not specify a Bazel version", u)
	}
	if len(result.ConfigsTarballDigest) == 0 {
		return nil, fmt.Errorf("manifest downloaded from %q did not specify a configs tarball digest", u)
	}
	return result, nil
}

// verifyConfigSHA verifies the sha256 digest of the config tarball in the downloaded manifest
// matches the digest of the configs tarball uploaded to the given URL. This function doesn't check
// if the uploaded configs is a valid tarball.
func verifyConfigSHA(m *rbeconfigsgen.Manifest, u string) error {
	resp, err := http.Get(u)
	if err != nil {
		return fmt.Errorf("unable to create a HTTP GET request to download the configs tarball from %q: %w", u, err)
	}
	defer resp.Body.Close()

	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return fmt.Errorf("error while downloading & hashing the contents of the configs tarball from %q: %w", u, err)
	}
	d := hex.EncodeToString(h.Sum(nil))
	if d != m.ConfigsTarballDigest {
		return fmt.Errorf("digest %s for configs tarball specified in downloaded manifest did not match digest %s computed by actually downloading the contents of configs tarball at %s", m.ConfigsTarballDigest, d, u)
	}
	return nil
}

// copyFile copies regular files from 'src' to 'dst' creating directories if necessary.
func copyFile(dst, src string) error {
	dir := path.Dir(dst)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create directory %q when copying %q to %q: %w", dir, src, dst, err)
	}
	i, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open %q for reading: %w", src, err)
	}
	defer i.Close()

	o, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to open %q for writing: %w", dst, err)
	}
	defer o.Close()

	if _, err := io.Copy(o, i); err != nil {
		return fmt.Errorf("error while copying the contents of %q to %q: %w", src, dst, err)
	}
	return nil
}

func createWorkspaceFile(m *rbeconfigsgen.Manifest, configTarballURL string, outputDir string) error {
	o, err := os.Create(path.Join(outputDir, "WORKSPACE"))
	if err != nil {
		return fmt.Errorf("unable to create WORKSPACE file in %q: %w", outputDir, err)
	}
	defer o.Close()
	data := struct {
		ConfigsTarballURL    string
		ConfigsTarballDigest string
	}{
		ConfigsTarballURL:    configTarballURL,
		ConfigsTarballDigest: m.ConfigsTarballDigest,
	}
	if err := workspaceTemplate.Execute(o, &data); err != nil {
		return fmt.Errorf("error writing Bazel WORKSPACE file in %q: %w", outputDir, err)
	}
	log.Printf("Generated WORKSPACE file in %q.", outputDir)
	return nil
}

func createBazelrcFile(m *rbeconfigsgen.Manifest, configTarballURL, outputDir, rbeInst string) error {
	o, err := os.Create(path.Join(outputDir, ".bazelrc"))
	if err != nil {
		return fmt.Errorf("unable to open .bazelrc file for writing in %q: %w", outputDir, err)
	}
	defer o.Close()

	fmt.Fprintf(o, `
# .bazelrc generated for:
#   Bazel %s
#   Toolchain Container %s (sha256:%s)
#   Configs Tarball URL %s (sha256:%s)
`, m.BazelVersion, m.ToolchainContainer, m.ImageDigest, configTarballURL, m.ConfigsTarballDigest)
	fmt.Fprintf(o, "\nbuild:remote --remote_instance_name=%s\n", rbeInst)
	fmt.Fprint(o, `
build:remote --jobs=6
build:remote --define=EXECUTOR=remote
build:remote --remote_executor=grpcs://remotebuildexecution.googleapis.com

# Enforce stricter environment rules, which eliminates some non-hermetic
# behavior and therefore improves both the remote cache hit rate and the
# correctness and repeatability of the build.
build:remote --incompatible_strict_action_env=true

build:remote --remote_timeout=3600

# Enable authentication. This will pick up application default credentials by
# default. You can use --google_credentials=some_file.json to use a service
# account credential instead.
build:remote --google_default_credentials=true

# C++ toolchain & default platform configuration.
build:remote --crosstool_top=@rbe_default//cc:toolchain
build:remote --action_env=BAZEL_DO_NOT_DETECT_CPP_TOOLCHAIN=1
build:remote --extra_toolchains=@rbe_default//config:cc-toolchain
build:remote --extra_execution_platforms=@rbe_default//config:platform
build:remote --host_platform=@rbe_default//config:platform
build:remote --platforms=@rbe_default//config:platform
`)
	// The Java toolchain rules used by Bazel are expected to change in a certain Bazel version
	// that affects the bazelrc file.
	u, err := rbeconfigsgen.UsesLocalJavaRuntime(m.BazelVersion)
	if err != nil {
		return fmt.Errorf("unable to determine type of Java toolchain rules used by Bazel %q: %w", m.BazelVersion, err)
	}
	if u {
		fmt.Fprint(o, `
build:remote --java_runtime_version=rbe_jdk
build:remote --tool_java_runtime_version=rbe_jdk
build:remote --extra_toolchains=@rbe_default//java:all
`)
	} else {
		fmt.Fprint(o, `
build:remote --host_javabase=@rbe_default//java:jdk
build:remote --javabase=@rbe_default//java:jdk
build:remote --host_java_toolchain=@bazel_tools//tools/jdk:toolchain_hostjdk8
build:remote --java_toolchain=@bazel_tools//tools/jdk:toolchain_hostjdk8
`)
	}
	log.Printf("Generated .bazelrc file in %q.", outputDir)
	return nil
}

// createTestRepo creates a Bazel repository that contains C++ & Java Hello World binary/test
// targets configured to run remotely on RBE using the toolchain configs from the given manifest &
// config tarball URL.
// Arguments:
// m is the manifest containing metadata about the toolchain configs being tested.
//
// configTarballURL is the URL to the remote toolchain configs tarball to be tested.
//
// srcDir is the path to the root of the locally cloned bazel-toolchains directory from where Hello
// World C++ & Java source files will be copied from.
//
// outputDir is the path where the Bazel repository configured to build Hello World on RBE will be
// created.
//
// rbeInst is the full name of the RBE instance the remote build will be run on.
func createTestRepo(m *rbeconfigsgen.Manifest, configTarballURL, srcDir, outputDir, rbeInst string) error {
	// For convenience only when locally running this test.
	log.Printf("DELETING the contents of output directory %q but ignoring any errors.", outputDir)
	os.RemoveAll(outputDir)
	if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
		return fmt.Errorf("unable to create output directory %q: %v", outputDir, err)
	}

	log.Printf("Copying C++ & Java Hello World source files from examples to the specified output directory.")
	for _, f := range filesToCopy {
		if err := copyFile(path.Join(outputDir, f), path.Join(srcDir, f)); err != nil {
			return fmt.Errorf("error copying %q from %q to %q: %v", f, srcDir, outputDir, err)
		}
		log.Printf("Copied %q from %q to %q.", f, srcDir, outputDir)
	}
	if err := createWorkspaceFile(m, configTarballURL, outputDir); err != nil {
		return fmt.Errorf("error creating the Bazel WORKSPACE file: %w", err)
	}
	if err := createBazelrcFile(m, configTarballURL, outputDir, rbeInst); err != nil {
		return fmt.Errorf("error creating the .bazelrc file: %w", err)
	}
	return nil
}

func validateRBEInstName(instName string) error {
	wantFormat := "projects/<GCP project ID>/instances/<instance ID>"
	splitName := strings.Split(instName, "/")
	if len(splitName) != 4 {
		return fmt.Errorf("%q did not conform to format %q because it split into %d elements by '/' instead of 4", instName, wantFormat, len(splitName))
	}
	if splitName[0] != "projects" {
		return fmt.Errorf("%q did not conform to format %q because the first element was %q instead of 'projects'", instName, wantFormat, splitName[0])
	}
	if splitName[2] != "instances" {
		return fmt.Errorf("%q did not conform to format %q because the third element was %q instead of 'instances'", instName, wantFormat, splitName[2])
	}

	return nil
}

// downloadBazelisk downloads Bazelisk for Linux to the given directory and returns the path to the
// downloaded Bazelisk executable.
func downloadBazelisk(outputDir string) (string, error) {
	bazeliskURL, bazeliskFile, err := rbeconfigsgen.BazeliskDownloadInfo(rbeconfigsgen.OSLinux)
	if err != nil {
		return "", fmt.Errorf("unable to determine URL to download Bazelisk from for Linux: %w", err)
	}
	resp, err := http.Get(bazeliskURL)
	if err != nil {
		return "", fmt.Errorf("unable to initialize the Bazelisk download from %q: %w", bazeliskURL, err)
	}
	defer resp.Body.Close()

	bazeliskPath := path.Join(outputDir, bazeliskFile)
	o, err := os.Create(bazeliskPath)
	defer o.Close()

	log.Printf("Downloading Bazelisk from %s to %s.", bazeliskURL, bazeliskPath)
	if _, err := io.Copy(o, resp.Body); err != nil {
		return "", fmt.Errorf("error while downloading Bazelisk from %q to %q: %w", bazeliskURL, bazeliskPath, err)
	}

	return bazeliskPath, nil

}

// runTestBuild runs the remote build using the toolchain configs using Bazelisk to pin the version
// of Bazel.
func runTestBuild(ctx context.Context, workingDir, bazelVersion string) error {
	bazeliskPath, err := downloadBazelisk(workingDir)
	if err != nil {
		return fmt.Errorf("failed to download Bazelisk: %w", err)
	}
	if err := os.Chmod(bazeliskPath, os.ModePerm); err != nil {
		return fmt.Errorf("unable to update the permissions of downloaded Bazelisk binary %q to make it executable: %w", bazeliskPath, err)
	}

	args := []string{
		// Use a custom output base to ensure Bazel runs with a clean local cache.
		fmt.Sprintf("--output_base=%s/.bazelcache", workingDir),
		"build",
		// This selects all the options specified in the .bazelrc file with config:remote.
		"--config=remote",
		// Disable remote caching to ensure the commands constructed from the toolchain configs
		// are actually valid.
		"--noremote_accept_cached",
		"//examples/..."}
	c := exec.CommandContext(ctx, bazeliskPath, args...)
	c.Env = append(c.Env, fmt.Sprintf("USE_BAZEL_VERSION=%s", bazelVersion))
	// Used by Bazelisk to determine where to download Bazel.
	c.Env = append(c.Env, fmt.Sprintf("XDG_CACHE_HOME=%s/.bazeliskcache", workingDir))
	c.Dir = workingDir
	log.Printf("Running '%s %s' with env %v with working directory %q.", bazeliskPath, strings.Join(args, " "), c.Env, workingDir)
	o, err := c.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("bazel build was killed because the timeout was reached")
	}
	if err != nil {
		log.Printf("Output from Bazel:\n%s", string(o))
		return fmt.Errorf("bazel build failed: %w", err)
	}
	return nil
}

func main() {
	flag.Parse()

	if len(*manifestURL) == 0 {
		log.Fatalf("--manifest_url was not specified.")
	}
	if len(*configsURL) == 0 {
		log.Fatalf("--configs_url was not specified.")
	}
	if len(*srcRoot) == 0 {
		log.Fatalf("--src_root was not specified.")
	}
	if len(*destRoot) == 0 {
		log.Fatalf("--dest_root was not specified.")
	}
	if len(*rbeInstance) == 0 {
		log.Fatalf("--rbe_instance was not specified.")
	}
	if err := validateRBEInstName(*rbeInstance); err != nil {
		log.Fatalf("--rbe_instance=%q was invalid: %v", *rbeInstance, err)
	}
	if *timeoutSeconds <= 0 {
		log.Fatalf("--timeout_seconds was either not specified or negative.")
	}

	log.Printf("--manifest_url=%q", *manifestURL)
	log.Printf("--configs_url=%q", *configsURL)
	log.Printf("--src_root=%q", *srcRoot)
	log.Printf("--dest_root=%q", *destRoot)
	log.Printf("--rbe_instance=%q", *rbeInstance)
	log.Printf("--timeout_seconds=%d", *timeoutSeconds)

	m, err := downloadManifest(*manifestURL)
	if err != nil {
		log.Fatalf("Unable to download the manifest from %q: %v", *manifestURL, err)
	}
	log.Printf("Successfully downloaded the JSON manifest from %s", *manifestURL)

	if err := verifyConfigSHA(m, *configsURL); err != nil {
		log.Fatalf("Failed to cross-reference configs digest specified in the manifest with the configs tarball: %v", err)
	}

	log.Printf("Creating a new Bazel test repository at %q.", *destRoot)

	if err := createTestRepo(m, *configsURL, *srcRoot, *destRoot, *rbeInstance); err != nil {
		log.Fatalf("Error creating the test Bazel repository: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSeconds)*time.Second)
	defer cancel()
	log.Printf("Running test build for Bazel %s using configs downloaded from %s with timeout set to %d seconds.", m.BazelVersion, *configsURL, *timeoutSeconds)
	if err := runTestBuild(ctx, *destRoot, m.BazelVersion); err != nil {
		log.Fatalf("Test build for Bazel %s using configs downloaded from %s failed on RBE Instance %s: %v", m.BazelVersion, *configsURL, *rbeInstance, err)
	}
	log.Printf("End to end test for toolchain configs for Bazel %s downloaded from %s passed on RBE Instance %s.", m.BazelVersion, *configsURL, *rbeInstance)
}
