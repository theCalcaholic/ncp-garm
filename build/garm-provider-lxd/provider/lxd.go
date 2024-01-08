// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright 2023 Cloudbase Solutions SRL
//
// Licensed under the AGPLv3, see LICENCE file for details

package provider

import (
	"context"
	"fmt"
	"sync"
	"time"

	runnerErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/execution"
	"github.com/cloudbase/garm-provider-lxd/config"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
	"github.com/pkg/errors"

	"github.com/cloudbase/garm-provider-common/cloudconfig"
	commonParams "github.com/cloudbase/garm-provider-common/params"
)

var _ execution.ExternalProvider = &LXD{}

const (
	// We look for this key in the config of the instances to determine if they are
	// created by us or not.
	controllerIDKeyName = "user.runner-controller-id"
	poolIDKey           = "user.runner-pool-id"

	// osTypeKeyName is the key we use in the instance config to indicate the OS
	// platform a runner is supposed to have. This value is defined in the pool and
	// passed into the provider as bootstrap params.
	osTypeKeyName = "user.os-type"

	// osArchKeyNAme is the key we use in the instance config to indicate the OS
	// architecture a runner is supposed to have. This value is defined in the pool and
	// passed into the provider as bootstrap params.
	osArchKeyNAme = "user.os-arch"
)

var (
	// lxdToGithubArchMap translates LXD architectures to Github tools architectures.
	// TODO: move this in a separate package. This will most likely be used
	// by any other provider.
	lxdToGithubArchMap map[string]string = map[string]string{
		"x86_64":  "x64",
		"amd64":   "x64",
		"armv7l":  "arm",
		"aarch64": "arm64",
		"x64":     "x64",
		"arm":     "arm",
		"arm64":   "arm64",
	}

	configToLXDArchMap map[commonParams.OSArch]string = map[commonParams.OSArch]string{
		commonParams.Amd64: "x86_64",
		commonParams.Arm64: "aarch64",
		commonParams.Arm:   "armv7l",
	}

	lxdToConfigArch map[string]commonParams.OSArch = map[string]commonParams.OSArch{
		"x86_64":  commonParams.Amd64,
		"aarch64": commonParams.Arm64,
		"armv7l":  commonParams.Arm,
	}
)

const (
	DefaultProjectDescription = "This project was created automatically by garm to be used for github ephemeral action runners."
	DefaultProjectName        = "garm-project"
)

func NewLXDProvider(configFile, controllerID string) (execution.ExternalProvider, error) {
	cfg, err := config.NewConfig(configFile)
	if err != nil {
		return nil, errors.Wrap(err, "parsing config")
	}
	if err := cfg.Validate(); err != nil {
		return nil, errors.Wrap(err, "validating provider config")
	}

	if len(cfg.ImageRemotes) == 0 {
		return nil, fmt.Errorf("no image remotes configured")
	}

	provider := &LXD{
		cfg:          cfg,
		controllerID: controllerID,
		imageManager: &image{
			remotes: cfg.ImageRemotes,
		},
	}

	return provider, nil
}

type LXD struct {
	// cfg is the provider config for this provider.
	cfg *config.LXD
	// cli is the LXD client.
	cli lxd.InstanceServer
	// imageManager downloads images from remotes
	imageManager *image
	// controllerID is the ID of this controller
	controllerID string

	mux sync.Mutex
}

func (l *LXD) getCLI(ctx context.Context) (lxd.InstanceServer, error) {
	l.mux.Lock()
	defer l.mux.Unlock()

	if l.cli != nil {
		return l.cli, nil
	}
	cli, err := getClientFromConfig(ctx, l.cfg)
	if err != nil {
		return nil, errors.Wrap(err, "creating LXD client")
	}

	_, _, err = cli.GetProject(projectName(l.cfg))
	if err != nil {
		return nil, errors.Wrapf(err, "fetching project name: %s", projectName(l.cfg))
	}
	cli = cli.UseProject(projectName(l.cfg))
	l.cli = cli

	return cli, nil
}

func (l *LXD) getProfiles(ctx context.Context, flavor string) ([]string, error) {
	ret := []string{}
	if l.cfg.IncludeDefaultProfile {
		ret = append(ret, "default")
	}

	set := map[string]struct{}{}

	cli, err := l.getCLI(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "fetching client")
	}

	profiles, err := cli.GetProfileNames()
	if err != nil {
		return nil, errors.Wrap(err, "fetching profile names")
	}
	for _, profile := range profiles {
		set[profile] = struct{}{}
	}

	if _, ok := set[flavor]; !ok {
		return nil, errors.Wrapf(runnerErrors.ErrNotFound, "looking for profile %s", flavor)
	}

	ret = append(ret, flavor)
	return ret, nil
}

func (l *LXD) getTools(tools []commonParams.RunnerApplicationDownload, osType commonParams.OSType, architecture string) (commonParams.RunnerApplicationDownload, error) {
	// Validate image OS. Linux only for now.
	switch osType {
	case commonParams.Linux:
	default:
		return commonParams.RunnerApplicationDownload{}, fmt.Errorf("this provider does not support OS type: %s", osType)
	}

	// Find tools for OS/Arch.
	for _, tool := range tools {
		if tool.GetOS() == "" || tool.GetArchitecture() == "" {
			continue
		}

		// fmt.Println(*tool.Architecture, *tool.OS)
		// fmt.Printf("image arch: %s --> osType: %s\n", image.Architecture, string(osType))
		if tool.GetArchitecture() == architecture && tool.GetOS() == string(osType) {
			return tool, nil
		}

		arch, ok := lxdToGithubArchMap[architecture]
		if ok && arch == tool.GetArchitecture() && tool.GetOS() == string(osType) {
			return tool, nil
		}
	}
	return commonParams.RunnerApplicationDownload{}, fmt.Errorf("failed to find tools for OS %s and arch %s", osType, architecture)
}

// sadly, the security.secureboot flag is a string encoded boolean.
func (l *LXD) secureBootEnabled() string {
	if l.cfg.SecureBoot {
		return "true"
	}
	return "false"
}

func (l *LXD) getCreateInstanceArgs(ctx context.Context, bootstrapParams commonParams.BootstrapInstance, specs extraSpecs) (api.InstancesPost, error) {
	if bootstrapParams.Name == "" {
		return api.InstancesPost{}, runnerErrors.NewBadRequestError("missing name")
	}
	profiles, err := l.getProfiles(ctx, bootstrapParams.Flavor)
	if err != nil {
		return api.InstancesPost{}, errors.Wrap(err, "fetching profiles")
	}

	arch, err := resolveArchitecture(bootstrapParams.OSArch)
	if err != nil {
		return api.InstancesPost{}, errors.Wrap(err, "fetching archictecture")
	}

	instanceType := l.cfg.GetInstanceType()
	instanceSource, err := l.imageManager.getInstanceSource(bootstrapParams.Image, instanceType, arch, l.cli)
	if err != nil {
		return api.InstancesPost{}, errors.Wrap(err, "getting instance source")
	}

	tools, err := l.getTools(bootstrapParams.Tools, bootstrapParams.OSType, arch)
	if err != nil {
		return api.InstancesPost{}, errors.Wrap(err, "getting tools")
	}

	bootstrapParams.UserDataOptions.DisableUpdatesOnBoot = specs.DisableUpdates
	bootstrapParams.UserDataOptions.ExtraPackages = specs.ExtraPackages
	bootstrapParams.UserDataOptions.EnableBootDebug = specs.EnableBootDebug
	cloudCfg, err := cloudconfig.GetCloudConfig(bootstrapParams, tools, bootstrapParams.Name)
	if err != nil {
		return api.InstancesPost{}, errors.Wrap(err, "generating cloud-config")
	}

	configMap := map[string]string{
		"user.user-data":    cloudCfg,
		osTypeKeyName:       string(bootstrapParams.OSType),
		osArchKeyNAme:       string(bootstrapParams.OSArch),
		controllerIDKeyName: l.controllerID,
		poolIDKey:           bootstrapParams.PoolID,
	}

	if instanceType == config.LXDImageVirtualMachine {
		configMap["security.secureboot"] = l.secureBootEnabled()
	}

	args := api.InstancesPost{
		InstancePut: api.InstancePut{
			Architecture: arch,
			Profiles:     profiles,
			Description:  "Github runner provisioned by garm",
			Config:       configMap,
		},
		Source: instanceSource,
		Name:   bootstrapParams.Name,
		Type:   api.InstanceType(instanceType),
	}
	return args, nil
}

func (l *LXD) launchInstance(ctx context.Context, createArgs api.InstancesPost) error {
	cli, err := l.getCLI(ctx)
	if err != nil {
		return errors.Wrap(err, "fetching client")
	}
	// Get LXD to create the instance (background operation)
	op, err := cli.CreateInstance(createArgs)
	if err != nil {
		return errors.Wrap(err, "creating instance")
	}

	// Wait for the operation to complete
	err = op.Wait()
	if err != nil {
		return errors.Wrap(err, "waiting for instance creation")
	}

	// Get LXD to start the instance (background operation)
	reqState := api.InstanceStatePut{
		Action:  "start",
		Timeout: -1,
	}

	op, err = cli.UpdateInstanceState(createArgs.Name, reqState, "")
	if err != nil {
		return errors.Wrap(err, "starting instance")
	}

	// Wait for the operation to complete
	err = op.Wait()
	if err != nil {
		return errors.Wrap(err, "waiting for instance to start")
	}
	return nil
}

// CreateInstance creates a new compute instance in the provider.
func (l *LXD) CreateInstance(ctx context.Context, bootstrapParams commonParams.BootstrapInstance) (commonParams.ProviderInstance, error) {
	extraSpecs, err := parseExtraSpecsFromBootstrapParams(bootstrapParams)
	if err != nil {
		return commonParams.ProviderInstance{}, errors.Wrap(err, "parsing extra specs")
	}
	args, err := l.getCreateInstanceArgs(ctx, bootstrapParams, extraSpecs)
	if err != nil {
		return commonParams.ProviderInstance{}, errors.Wrap(err, "fetching create args")
	}

	if err := l.launchInstance(ctx, args); err != nil {
		return commonParams.ProviderInstance{}, errors.Wrap(err, "creating instance")
	}

	ret, err := l.waitInstanceHasIP(ctx, args.Name)
	if err != nil {
		return commonParams.ProviderInstance{}, errors.Wrap(err, "fetching instance")
	}

	return ret, nil
}

// GetInstance will return details about one instance.
func (l *LXD) GetInstance(ctx context.Context, instanceName string) (commonParams.ProviderInstance, error) {
	cli, err := l.getCLI(ctx)
	if err != nil {
		return commonParams.ProviderInstance{}, errors.Wrap(err, "fetching client")
	}
	instance, _, err := cli.GetInstanceFull(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return commonParams.ProviderInstance{}, errors.Wrapf(runnerErrors.ErrNotFound, "fetching instance: %q", err)
		}
		return commonParams.ProviderInstance{}, errors.Wrap(err, "fetching instance")
	}

	return lxdInstanceToAPIInstance(instance), nil
}

// Delete instance will delete the instance in a provider.
func (l *LXD) DeleteInstance(ctx context.Context, instance string) error {
	cli, err := l.getCLI(ctx)
	if err != nil {
		return errors.Wrap(err, "fetching client")
	}

	if err := l.setState(ctx, instance, "stop", true); err != nil {
		if isNotFoundError(err) {
			return nil
		}
		// I am not proud of this, but the drivers.ErrInstanceIsStopped from LXD pulls in
		// a ton of CGO, linux specific dependencies, that don't make sense having
		// in garm.
		if !(errors.Cause(err).Error() == errInstanceIsStopped.Error()) {
			return errors.Wrap(err, "stopping instance")
		}
	}

	opResponse := make(chan struct {
		op  lxd.Operation
		err error
	})
	var op lxd.Operation
	go func() {
		op, err := cli.DeleteInstance(instance)
		opResponse <- struct {
			op  lxd.Operation
			err error
		}{op: op, err: err}
	}()

	select {
	case resp := <-opResponse:
		if resp.err != nil {
			if isNotFoundError(resp.err) {
				return nil
			}
			return errors.Wrap(resp.err, "removing instance")
		}
		op = resp.op
	case <-time.After(time.Second * 60):
		return errors.Wrapf(runnerErrors.ErrTimeout, "removing instance %s", instance)
	}

	opTimeout, cancel := context.WithTimeout(context.Background(), time.Second*60)
	defer cancel()
	err = op.WaitContext(opTimeout)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return errors.Wrap(err, "waiting for instance deletion")
	}
	return nil
}

type listResponse struct {
	instances []api.InstanceFull
	err       error
}

// ListInstances will list all instances for a provider.
func (l *LXD) ListInstances(ctx context.Context, poolID string) ([]commonParams.ProviderInstance, error) {
	cli, err := l.getCLI(ctx)
	if err != nil {
		return []commonParams.ProviderInstance{}, errors.Wrap(err, "fetching client")
	}

	result := make(chan listResponse, 1)

	go func() {
		// TODO(gabriel-samfira): if this blocks indefinitely, we will leak a goroutine.
		// Convert the internal provider to an external one. Running the provider as an
		// external process will allow us to not care if a goroutine leaks. Once a timeout
		// is reached, the provider can just exit with an error. Something we can't do with
		// internal providers.
		instances, err := cli.GetInstancesFull(api.InstanceTypeAny)
		result <- listResponse{
			instances: instances,
			err:       err,
		}
	}()

	var instances []api.InstanceFull
	select {
	case res := <-result:
		if res.err != nil {
			return []commonParams.ProviderInstance{}, errors.Wrap(res.err, "fetching instances")
		}
		instances = res.instances
	case <-time.After(time.Second * 60):
		return []commonParams.ProviderInstance{}, errors.Wrap(runnerErrors.ErrTimeout, "fetching instances from provider")
	}

	ret := []commonParams.ProviderInstance{}

	for _, instance := range instances {
		if id, ok := instance.ExpandedConfig[controllerIDKeyName]; ok && id == l.controllerID {
			if poolID != "" {
				id := instance.ExpandedConfig[poolIDKey]
				if id != poolID {
					// Pool ID was specified. Filter out instances belonging to other pools.
					continue
				}
			}
			ret = append(ret, lxdInstanceToAPIInstance(&instance))
		}
	}

	return ret, nil
}

// RemoveAllInstances will remove all instances created by this provider.
func (l *LXD) RemoveAllInstances(ctx context.Context) error {
	instances, err := l.ListInstances(ctx, "")
	if err != nil {
		return errors.Wrap(err, "fetching instance list")
	}

	for _, instance := range instances {
		// TODO: remove in parallel
		if err := l.DeleteInstance(ctx, instance.Name); err != nil {
			return errors.Wrapf(err, "removing instance %s", instance.Name)
		}
	}

	return nil
}

func (l *LXD) setState(ctx context.Context, instance, state string, force bool) error {
	reqState := api.InstanceStatePut{
		Action:  state,
		Timeout: -1,
		Force:   force,
	}

	cli, err := l.getCLI(ctx)
	if err != nil {
		return errors.Wrap(err, "fetching client")
	}

	op, err := cli.UpdateInstanceState(instance, reqState, "")
	if err != nil {
		return errors.Wrapf(err, "setting state to %s", state)
	}
	ctxTimeout, cancel := context.WithTimeout(context.Background(), time.Second*60)
	defer cancel()
	err = op.WaitContext(ctxTimeout)
	if err != nil {
		return errors.Wrapf(err, "waiting for instance to transition to state %s", state)
	}
	return nil
}

// Stop shuts down the instance.
func (l *LXD) Stop(ctx context.Context, instance string, force bool) error {
	return l.setState(ctx, instance, "stop", force)
}

// Start boots up an instance.
func (l *LXD) Start(ctx context.Context, instance string) error {
	return l.setState(ctx, instance, "start", false)
}
