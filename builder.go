package lifecycle

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/pkg/errors"

	"github.com/buildpacks/lifecycle/api"
	"github.com/buildpacks/lifecycle/launch"
	"github.com/buildpacks/lifecycle/layers"
)

type Builder struct {
	AppDir        string
	LayersDir     string
	PlatformDir   string
	BuildpacksDir string
	PlatformAPI   *api.Version
	Env           BuildEnv
	Group         BuildpackGroup
	Plan          BuildPlan
	Out, Err      io.Writer
}

type BuildEnv interface {
	AddRootDir(baseDir string) error
	AddEnvDir(envDir string) error
	WithPlatform(platformDir string) ([]string, error)
	List() []string
}

type BuildTOML struct {
	BOM   []BOMEntry `toml:"bom"`
	Unmet []Unmet    `toml:"unmet"`
}

type Unmet struct {
	Name string `toml:"name"`
}

type LaunchTOML struct {
	BOM       []BOMEntry
	Labels    []Label
	Processes []launch.Process `toml:"processes"`
	Slices    []layers.Slice   `toml:"slices"`
}

type BOMEntry struct {
	Require
	Buildpack Buildpack `toml:"buildpack" json:"buildpack"`
}

type Label struct {
	Key   string `toml:"key"`
	Value string `toml:"value"`
}

type BuildpackPlan struct {
	Entries []Require `toml:"entries"`
}

type BuildResult struct {
	BOM       []BOMEntry
	Labels    []Label
	Met       []Require
	Processes []launch.Process
	Slices    []layers.Slice
}

func (b *Builder) Build() (*BuildMetadata, error) {
	config, err := b.BuildConfig()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(config.PlanDir)

	procMap := processMap{}
	plan := b.Plan
	var bom []BOMEntry
	var slices []layers.Slice
	var labels []Label

	for _, bp := range b.Group.Group {
		bpTOML, err := bp.Lookup(b.BuildpacksDir)
		if err != nil {
			return nil, err
		}

		br, err := bpTOML.Build(bp, plan, config)
		if err != nil {
			return nil, err
		}

		bom = append(bom, br.BOM...)
		labels = append(labels, br.Labels...)
		plan = plan.filter(br.Met)
		procMap.add(br.Processes)
		slices = append(slices, br.Slices...)
	}

	if b.PlatformAPI.Compare(api.MustParse("0.4")) < 0 { // PlatformAPI <= 0.3
		for i := range bom {
			if err := bom[i].convertMetadataToVersion(); err != nil {
				return nil, err
			}
		}
	} else {
		for i := range bom {
			if err := bom[i].convertVersionToMetadata(); err != nil {
				return nil, err
			}
		}
	}

	return &BuildMetadata{
		BOM:        bom,
		Buildpacks: b.Group.Group,
		Labels:     labels,
		Processes:  procMap.list(),
		Slices:     slices,
	}, nil
}

type BuildConfig struct {
	Env         BuildEnv
	AppDir      string
	PlatformDir string
	LayersDir   string
	PlanDir     string
	Out         io.Writer
	Err         io.Writer
}

func (b *Builder) BuildConfig() (BuildConfig, error) {
	appDir, err := filepath.Abs(b.AppDir)
	if err != nil {
		return BuildConfig{}, err
	}
	platformDir, err := filepath.Abs(b.PlatformDir)
	if err != nil {
		return BuildConfig{}, err
	}
	layersDir, err := filepath.Abs(b.LayersDir)
	if err != nil {
		return BuildConfig{}, err
	}
	planDir, err := ioutil.TempDir("", "plan.")
	if err != nil {
		return BuildConfig{}, err
	}

	return BuildConfig{
		Env:         b.Env,
		AppDir:      appDir,
		PlatformDir: platformDir,
		LayersDir:   layersDir,
		PlanDir:     planDir,
		Out:         b.Out,
		Err:         b.Err,
	}, nil
}

func (b *BuildpackTOML) Build(bp Buildpack, plan BuildPlan, config BuildConfig) (BuildResult, error) {
	foundPlan := plan.find(bp.noAPI().noHomepage())
	if api.MustParse(bp.API).Equal(api.MustParse("0.2")) {
		for i := range foundPlan.Entries {
			foundPlan.Entries[i].convertMetadataToVersion()
		}
	}

	bpLayersDir, bpPlanPath, err := preparePaths(bp, foundPlan, config.LayersDir, config.PlanDir)
	if err != nil {
		return BuildResult{}, err
	}

	if err := b.runBuildCmd(bpLayersDir, bpPlanPath, config); err != nil {
		return BuildResult{}, err
	}

	if err := setupEnv(config.Env, bpLayersDir); err != nil {
		return BuildResult{}, err
	}

	return readOutputFiles(bp, bpLayersDir, bpPlanPath, foundPlan)
}

func (b *BuildpackTOML) runBuildCmd(bpLayersDir, bpPlanPath string, config BuildConfig) error {
	cmd := exec.Command(
		filepath.Join(b.Path, "bin", "build"),
		bpLayersDir,
		config.PlatformDir,
		bpPlanPath,
	)
	cmd.Dir = config.AppDir
	cmd.Stdout = config.Out
	cmd.Stderr = config.Err

	var err error
	if b.Buildpack.ClearEnv {
		cmd.Env = config.Env.List()
	} else {
		cmd.Env, err = config.Env.WithPlatform(config.PlatformDir)
		if err != nil {
			return err
		}
	}
	cmd.Env = append(cmd.Env, EnvBuildpackDir+"="+b.Path)

	if err := cmd.Run(); err != nil {
		return NewLifecycleError(err, ErrTypeBuildpack)
	}
	return nil
}

func preparePaths(bp Buildpack, foundPlan BuildpackPlan, layersDir, planDir string) (string, string, error) {
	bpDirName := launch.EscapeID(bp.ID)
	bpLayersDir := filepath.Join(layersDir, bpDirName)
	bpPlanDir := filepath.Join(planDir, bpDirName)
	if err := os.MkdirAll(bpLayersDir, 0777); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(bpPlanDir, 0777); err != nil {
		return "", "", err
	}
	bpPlanPath := filepath.Join(bpPlanDir, "plan.toml")
	if err := WriteTOML(bpPlanPath, foundPlan); err != nil {
		return "", "", err
	}

	return bpLayersDir, bpPlanPath, nil
}

func readOutputFiles(bp Buildpack, bpLayersDir, bpPlanPath string, bpPlanIn BuildpackPlan) (BuildResult, error) {
	// read buildpack plan
	var bpPlanOut BuildpackPlan
	if _, err := toml.DecodeFile(bpPlanPath, &bpPlanOut); err != nil {
		return BuildResult{}, err
	}
	// read build.toml
	var bpBuild BuildTOML
	if api.MustParse(bp.API).Compare(api.MustParse("0.5")) >= 0 { // bp.API >= 0.5
		buildPath := filepath.Join(bpLayersDir, "build.toml")
		if _, err := toml.DecodeFile(buildPath, &bpBuild); err != nil && !os.IsNotExist(err) {
			return BuildResult{}, err
		}
		if err := validateBuildTOML(bpBuild, bpPlanIn); err != nil {
			return BuildResult{}, err
		}
	}
	// read launch.toml
	var launch LaunchTOML
	launchPath := filepath.Join(bpLayersDir, "launch.toml")
	if _, err := toml.DecodeFile(launchPath, &launch); err != nil && !os.IsNotExist(err) {
		return BuildResult{}, err
	}
	// setup result
	var br BuildResult
	if api.MustParse(bp.API).Compare(api.MustParse("0.5")) < 0 { // bp.API <= 0.4
		br.BOM = withBuildpack(bp, bpPlanOut.toBOM())
		br.Met = bpPlanOut.Entries
	} else {
		br.BOM = withBuildpack(bp, launch.BOM)
		br.Met = bpPlanIn.filter(bpBuild.Unmet).Entries
	}
	br.Labels = launch.Labels
	for i := range launch.Processes {
		launch.Processes[i].BuildpackID = bp.ID
	}
	br.Processes = launch.Processes
	br.Slices = launch.Slices

	return br, nil
}

func validateBuildTOML(buildTOML BuildTOML, bpPlan BuildpackPlan) error {
	for _, unmet := range buildTOML.Unmet {
		if unmet.Name == "" {
			return errors.New("unmet.name is required")
		}
		found := false
		for _, req := range bpPlan.Entries {
			if unmet.Name == req.Name {
				found = true
				break
			}
		}
		if !found {
			return errors.New(fmt.Sprintf("unmet.name '%s' must match a requested dependency", unmet.Name))
		}
	}
	return nil
}

func withBuildpack(bp Buildpack, bom []BOMEntry) []BOMEntry {
	var out []BOMEntry
	for _, entry := range bom {
		entry.Buildpack = bp.noAPI().noHomepage()
		out = append(out, entry)
	}
	return out
}

func setupEnv(env BuildEnv, layersDir string) error {
	if err := eachDir(layersDir, func(path string) error {
		if !isBuild(path + ".toml") {
			return nil
		}
		return env.AddRootDir(path)
	}); err != nil {
		return err
	}

	return eachDir(layersDir, func(path string) error {
		if !isBuild(path + ".toml") {
			return nil
		}
		if err := env.AddEnvDir(filepath.Join(path, "env")); err != nil {
			return err
		}
		return env.AddEnvDir(filepath.Join(path, "env.build"))
	})
}

func eachDir(dir string, fn func(path string) error) error {
	files, err := ioutil.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	for _, f := range files {
		if !f.IsDir() {
			continue
		}
		if err := fn(filepath.Join(dir, f.Name())); err != nil {
			return err
		}
	}
	return nil
}

func isBuild(path string) bool {
	var layerTOML struct {
		Build bool `toml:"build"`
	}
	_, err := toml.DecodeFile(path, &layerTOML)
	return err == nil && layerTOML.Build
}

func (p BuildPlan) find(bp Buildpack) BuildpackPlan {
	var out []Require
	for _, entry := range p.Entries {
		for _, provider := range entry.Providers {
			if provider == bp {
				out = append(out, entry.Requires...)
				break
			}
		}
	}
	return BuildpackPlan{Entries: out}
}

// TODO: ensure at least one claimed entry of each name is provided by the BP
func (p BuildPlan) filter(met []Require) BuildPlan {
	var out []BuildPlanEntry
	for _, planEntry := range p.Entries {
		if !containsEntry(met, planEntry) {
			out = append(out, planEntry)
		}
	}
	return BuildPlan{Entries: out}
}

func containsEntry(requires []Require, entry BuildPlanEntry) bool {
	for _, req := range requires {
		for _, planReq := range entry.Requires {
			if req.Name == planReq.Name {
				return true
			}
		}
	}
	return false
}

func (p BuildpackPlan) filter(unmet []Unmet) BuildpackPlan {
	var out []Require
	for _, entry := range p.Entries {
		if !containsName(unmet, entry.Name) {
			out = append(out, entry)
		}
	}
	return BuildpackPlan{Entries: out}
}

func containsName(unmet []Unmet, name string) bool {
	for _, u := range unmet {
		if u.Name == name {
			return true
		}
	}
	return false
}

func (p BuildpackPlan) toBOM() []BOMEntry {
	var bom []BOMEntry
	for _, entry := range p.Entries {
		bom = append(bom, BOMEntry{Require: entry})
	}
	return bom
}

func (bom *BOMEntry) convertMetadataToVersion() error {
	if version, ok := bom.Metadata["version"]; ok {
		metadataVersion := fmt.Sprintf("%v", version)
		if bom.Version != "" && bom.Version != metadataVersion {
			return errors.New("top level version does not match metadata version")
		}
		bom.Version = metadataVersion
	}
	return nil
}

func (bom *BOMEntry) convertVersionToMetadata() error {
	if bom.Version != "" {
		if bom.Metadata == nil {
			bom.Metadata = make(map[string]interface{})
		}
		if version, ok := bom.Metadata["version"]; ok {
			metadataVersion := fmt.Sprintf("%v", version)
			if metadataVersion != "" && metadataVersion != bom.Version {
				return errors.New("metadata version does not match top level version")
			}
		}
		bom.Metadata["version"] = bom.Version
		bom.Version = ""
	}
	return nil
}

type processMap map[string]launch.Process

func (m processMap) add(l []launch.Process) {
	for _, proc := range l {
		m[proc.Type] = proc
	}
}

func (m processMap) list() []launch.Process {
	var keys []string
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	procs := []launch.Process{}
	for _, key := range keys {
		procs = append(procs, m[key])
	}
	return procs
}
