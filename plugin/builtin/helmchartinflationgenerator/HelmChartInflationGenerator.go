// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

// Helm chart inflation generator.
// Uses helm V3 to generate k8s YAML from a helm chart.

//go:generate pluginator
package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/imdario/mergo"
	"github.com/pkg/errors"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"
)

// HelmChartInflationGeneratorPlugin is a plugin to generate resources
// from a remote or local helm chart.
type HelmChartInflationGeneratorPlugin struct {
	h *resmap.PluginHelpers
	types.HelmGlobals
	types.HelmChart
	tmpDir string
}

//noinspection GoUnusedGlobalVariable
var KustomizePlugin HelmChartInflationGeneratorPlugin

const (
	valuesMergeOptionMerge    = "merge"
	valuesMergeOptionOverride = "override"
	valuesMergeOptionReplace  = "replace"
)

var legalMergeOptions = []string{
	valuesMergeOptionMerge,
	valuesMergeOptionOverride,
	valuesMergeOptionReplace,
}

// Config uses the input plugin configurations `config` to setup the generator
// options
func (p *HelmChartInflationGeneratorPlugin) Config(
	h *resmap.PluginHelpers, config []byte) (err error) {
	if h.GeneralConfig() == nil {
		return fmt.Errorf("unable to access general config")
	}
	if !h.GeneralConfig().HelmConfig.Enabled {
		return fmt.Errorf("must specify --enable-helm")
	}
	if h.GeneralConfig().HelmConfig.Command == "" {
		return fmt.Errorf("must specify --helm-command")
	}
	p.h = h
	if err = yaml.Unmarshal(config, p); err != nil {
		return
	}
	return p.validateArgs()
}

// This uses the real file system since tmpDir may be used
// by the helm subprocess.  Cannot use a chroot jail or fake
// filesystem since we allow the user to use previously
// downloaded charts.  This is safe since this plugin is
// owned by kustomize.
func (p *HelmChartInflationGeneratorPlugin) establishTmpDir() (err error) {
	if p.tmpDir != "" {
		// already done.
		return nil
	}
	p.tmpDir, err = ioutil.TempDir("", "kustomize-helm-")
	return err
}

func (p *HelmChartInflationGeneratorPlugin) validateArgs() (err error) {
	if p.Name == "" {
		return fmt.Errorf("chart name cannot be empty")
	}

	// ChartHome might be consulted by the plugin (to read
	// values files below it), so it must be located under
	// the loader root (unless root restrictions are
	// disabled, in which case this can be an absolute path).
	if p.ChartHome == "" {
		p.ChartHome = "charts"
	}

	// The ValuesFile may be consulted by the plugin, so it must
	// be under the loader root (unless root restrictions are
	// disabled).
	if p.ValuesFile == "" {
		p.ValuesFile = filepath.Join(p.ChartHome, p.Name, "values.yaml")
	}

	if err = p.errIfIllegalValuesMerge(); err != nil {
		return err
	}

	// ConfigHome is not loaded by the plugin, and can be located anywhere.
	if p.ConfigHome == "" {
		if err = p.establishTmpDir(); err != nil {
			return errors.Wrap(
				err, "unable to create tmp dir for HELM_CONFIG_HOME")
		}
		p.ConfigHome = filepath.Join(p.tmpDir, "helm")
	}
	return nil
}

func (p *HelmChartInflationGeneratorPlugin) errIfIllegalValuesMerge() error {
	if p.ValuesMerge == "" {
		// Use the default.
		p.ValuesMerge = valuesMergeOptionOverride
		return nil
	}
	for _, opt := range legalMergeOptions {
		if p.ValuesMerge == opt {
			return nil
		}
	}
	return fmt.Errorf("valuesMerge must be one of %v", legalMergeOptions)
}

func (p *HelmChartInflationGeneratorPlugin) absChartHome() string {
	if filepath.IsAbs(p.ChartHome) {
		return p.ChartHome
	}
	return filepath.Join(p.h.Loader().Root(), p.ChartHome)
}

func (p *HelmChartInflationGeneratorPlugin) runHelmCommand(
	args []string) ([]byte, error) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	cmd := exec.Command(p.h.GeneralConfig().HelmConfig.Command, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	env := []string{
		fmt.Sprintf("HELM_CONFIG_HOME=%s", p.ConfigHome),
		fmt.Sprintf("HELM_CACHE_HOME=%s/.cache", p.ConfigHome),
		fmt.Sprintf("HELM_DATA_HOME=%s/.data", p.ConfigHome)}
	cmd.Env = append(os.Environ(), env...)
	err := cmd.Run()
	if err != nil {
		helm := p.h.GeneralConfig().HelmConfig.Command
		err = errors.Wrap(
			fmt.Errorf(
				"unable to run: '%s %s' with env=%s (is '%s' installed?)",
				helm, strings.Join(args, " "), env, helm),
			stderr.String(),
		)
	}
	return stdout.Bytes(), err
}

// createNewMergedValuesFile replaces/merges original values file with ValuesInline.
func (p *HelmChartInflationGeneratorPlugin) createNewMergedValuesFile() (
	path string, err error) {
	if p.ValuesMerge == valuesMergeOptionMerge ||
		p.ValuesMerge == valuesMergeOptionOverride {
		if err = p.replaceValuesInline(); err != nil {
			return "", err
		}
	}
	var b []byte
	b, err = yaml.Marshal(p.ValuesInline)
	if err != nil {
		return "", err
	}
	return p.writeValuesBytes(b)
}

func (p *HelmChartInflationGeneratorPlugin) replaceValuesInline() error {
	pValues, err := p.h.Loader().Load(p.ValuesFile)
	if err != nil {
		return err
	}
	chValues := make(map[string]interface{})
	if err = yaml.Unmarshal(pValues, &chValues); err != nil {
		return err
	}
	switch p.ValuesMerge {
	case valuesMergeOptionOverride:
		err = mergo.Merge(
			&chValues, p.ValuesInline, mergo.WithOverride)
	case valuesMergeOptionMerge:
		err = mergo.Merge(&chValues, p.ValuesInline)
	}
	p.ValuesInline = chValues
	return err
}

// copyValuesFile to avoid branching.  TODO: get rid of this.
func (p *HelmChartInflationGeneratorPlugin) copyValuesFile() (string, error) {
	b, err := p.h.Loader().Load(p.ValuesFile)
	if err != nil {
		return "", err
	}
	return p.writeValuesBytes(b)
}

// Write a absolute path file in the tmp file system.
func (p *HelmChartInflationGeneratorPlugin) writeValuesBytes(
	b []byte) (string, error) {
	if err := p.establishTmpDir(); err != nil {
		return "", fmt.Errorf("cannot create tmp dir to write helm values")
	}
	path := filepath.Join(p.tmpDir, p.Name+"-kustomize-values.yaml")
	return path, ioutil.WriteFile(path, b, 0644)
}

func (p *HelmChartInflationGeneratorPlugin) cleanup() {
	if p.tmpDir != "" {
		os.RemoveAll(p.tmpDir)
	}
}

// Generate implements generator
func (p *HelmChartInflationGeneratorPlugin) Generate() (rm resmap.ResMap, err error) {
	defer p.cleanup()
	if err = p.checkHelmVersion(); err != nil {
		return nil, err
	}
	if path, exists := p.chartExistsLocally(); !exists {
		if p.Repo == "" {
			return nil, fmt.Errorf(
				"no repo specified for pull, no chart found at '%s'", path)
		}
		if _, err := p.runHelmCommand(p.pullCommand()); err != nil {
			return nil, err
		}
	}
	if len(p.ValuesInline) > 0 {
		p.ValuesFile, err = p.createNewMergedValuesFile()
	} else {
		p.ValuesFile, err = p.copyValuesFile()
	}
	if err != nil {
		return nil, err
	}
	var stdout []byte
	stdout, err = p.runHelmCommand(p.templateCommand())
	if err != nil {
		return nil, err
	}

	rm, err = p.h.ResmapFactory().NewResMapFromBytes(stdout)
	if err == nil {
		return rm, nil
	}
	// try to remove the contents before first "---" because
	// helm may produce messages to stdout before it
	stdoutStr := string(stdout)
	if idx := strings.Index(stdoutStr, "---"); idx != -1 {
		return p.h.ResmapFactory().NewResMapFromBytes([]byte(stdoutStr[idx:]))
	}
	return nil, err
}

func (p *HelmChartInflationGeneratorPlugin) templateCommand() []string {
	args := []string{"template"}
	if p.ReleaseName != "" {
		args = append(args, p.ReleaseName)
	}
	if p.Namespace != "" {
		args = append(args, "--namespace", p.Namespace)
	}
	args = append(args, filepath.Join(p.absChartHome(), p.Name))
	if p.ValuesFile != "" {
		args = append(args, "--values", p.ValuesFile)
	}
	if p.ReleaseName == "" {
		// AFAICT, this doesn't work as intended due to a bug in helm.
		// See https://github.com/helm/helm/issues/6019
		// I've tried placing the flag before and after the name argument.
		args = append(args, "--generate-name")
	}
	if p.IncludeCRDs {
		args = append(args, "--include-crds")
	}
	return args
}

func (p *HelmChartInflationGeneratorPlugin) pullCommand() []string {
	args := []string{
		"pull",
		"--untar",
		"--untardir", p.absChartHome(),
		"--repo", p.Repo,
		p.Name}
	if p.Version != "" {
		args = append(args, "--version", p.Version)
	}
	return args
}

// chartExistsLocally will return true if the chart does exist in
// local chart home.
func (p *HelmChartInflationGeneratorPlugin) chartExistsLocally() (string, bool) {
	path := filepath.Join(p.absChartHome(), p.Name)
	s, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return path, s.IsDir()
}

// checkHelmVersion will return an error if the helm version is not V3
func (p *HelmChartInflationGeneratorPlugin) checkHelmVersion() error {
	stdout, err := p.runHelmCommand([]string{"version", "-c", "--short"})
	if err != nil {
		return err
	}
	r, err := regexp.Compile(`v?\d+(\.\d+)+`)
	if err != nil {
		return err
	}
	v := r.FindString(string(stdout))
	if v == "" {
		return fmt.Errorf("cannot find version string in %s", string(stdout))
	}
	if v[0] == 'v' {
		v = v[1:]
	}
	majorVersion := strings.Split(v, ".")[0]
	if majorVersion != "3" {
		return fmt.Errorf("this plugin requires helm V3 but got v%s", v)
	}
	return nil
}