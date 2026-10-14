// SPDX-License-Identifier: GPL-3.0-only
//
// This file is part of the podman-launcher project:
//
//	https://github.com/89luca89/podman-launcher
//
// # Copyright (C) 2023 podman-launcher contributors
//
// podman-launcher is free software; you can redistribute it and/or modify it
// under the terms of the GNU General Public License version 3
// as published by the Free Software Foundation.
//
// podman-launcher is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
// General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with podman-launcher; if not, see <http://www.gnu.org/licenses/>.

// Package launcher is a library to help ship the latest release of
//
//	https://github.com/mgoltzsche/podman-static/
//
// This repo builds and releases all podman components as statically linked binaries
// this will let us to easily ship the container manager without needing all the
// dependency resolution of a package manager.
// To make it work properly we need also to setup some variables, configs and paths.
// This program will take care of that, and will make sure that the podman configuration
// does not overlap with the one eventually installed by a package manager, making
// this iteration of podman isolated from the rest.
package launcher

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
)

// Config is the struct holding the current podman-launcher assets and configuration.
// Be sure to initialize it using NewLauncher() and pass the proper pack payload.
type Config struct {
	pack                   []byte
	targetDir              string
	tmpDir                 string
	runtimeDir             string
	containersConf         string
	containersRegistryConf string
	containersStorageConf  string
	containersPolicyJSON   string
}

// populate our container.conf file using the template given.
func (conf *Config) setupContainerConf() error {
	containerConf := `[containers]
init_path = "{{.Path}}/lib/podman/catatonit"
[engine]
conmon_env_vars = [
    "CONTAINERS_CONF={{.Path}}/conf/containers/containers.conf",
    "CONTAINERS_REGISTRIES_CONF={{.Path}}/conf/containers/registries.conf",
    "CONTAINERS_STORAGE_CONF={{.Path}}/conf/containers/storage.conf",
    "XDG_RUNTIME_DIR={{.RuntimeDir}}",
    "PATH={{.Path}}/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
]
conmon_path=[ "{{.Path}}/lib/podman/conmon" ]
helper_binaries_dir = [ "{{.Path}}/lib/podman" ]
runtime = "crun"
network_cmd_path = "{{.Path}}/bin/slirp4netns"
static_dir = "{{.Path}}/share/podman/libpod"
volume_path = "{{.Path}}/share/podman/volume"
[engine.runtimes]
crun = [ "{{.Path}}/bin/crun" ]
runc = [ "{{.Path}}/bin/runc" ]
[network]
cni_plugin_dirs = [ "{{.Path}}/lib/cni" ]`

	tmpl, err := template.New("conf").Parse(containerConf)
	if err != nil {
		return err
	}

	// set the Path to our targetDir
	vars := make(map[string]interface{})
	vars["Path"] = conf.targetDir
	vars["RuntimeDir"] = conf.runtimeDir

	// and save it
	file, err := os.Create(conf.containersConf)
	if err != nil {
		return err
	}

	return tmpl.Execute(file, vars)
}

// setup storage.conf in order to point to our targetDIR and binaries correctly.
func (conf *Config) setupStorageConf() error {
	storageConf, err := os.ReadFile(conf.containersStorageConf)
	if err != nil {
		return err
	}

	// Replace /var with our directory, and point to our fuse-overlayfs binary
	content := bytes.ReplaceAll(storageConf, []byte("/var"), []byte(conf.targetDir))
	content = bytes.ReplaceAll(content,
		[]byte("/usr/local/bin/fuse-overlayfs"),
		[]byte(filepath.Join(conf.targetDir, "bin/fuse-overlayfs")))
	// and save the config file
	err = os.WriteFile(conf.containersStorageConf, content, 0o600)
	if err != nil {
		return err
	}

	return nil
}

func (conf *Config) setupConfs() error {
	// if we already ran the first setup, we don't overwrite the configs
	_, errStorageConf := os.Stat(conf.containersStorageConf)
	_, errConf := os.Stat(conf.containersConf)

	_, err := os.Stat(filepath.Join(conf.targetDir, "conf"))
	if err != nil {
		// if we didn't then copy the default configs from etc into conf and set them up
		err = exec.Command("cp", "-r", conf.targetDir+"/etc", conf.targetDir+"/conf").Run()
		if err != nil {
			return err
		}
	}

	if errStorageConf != nil {
		err = exec.Command("cp", "-r", conf.targetDir+"/etc/containers/storage.conf", conf.containersStorageConf).Run()
		if err != nil {
			return err
		}

		err = conf.setupStorageConf()
		if err != nil {
			return err
		}
	}

	if errConf != nil {
		err = exec.Command("cp", "-r", conf.targetDir+"/etc/containers/containers.conf", conf.containersConf).Run()
		if err != nil {
			return err
		}

		err = conf.setupContainerConf()
		if err != nil {
			return err
		}
	}

	return nil
}

// unpackedMarker is the file whose presence says the payload has been unpacked
// and the configs set up.
func (conf *Config) unpackedMarker() string {
	return filepath.Join(conf.targetDir, ".unpacked")
}

func (conf *Config) prepareFiles() error {
	// create our unpack dir
	err := os.MkdirAll(conf.targetDir, 0o755)
	if err != nil {
		return err
	}

	// we need to make sure the runtime dir is present, or podman will complain
	err = os.MkdirAll(conf.runtimeDir, 0o755)
	if err != nil {
		return err
	}

	// The payload is unpacked once, and this marker is how the next invocation
	// knows. Checking for a list of files instead - as this used to - unpacks
	// the payload again on every invocation as soon as that list names a file
	// the payload does not ship, and this podman-static comes with netavark and
	// pasta rather than lib/cni and slirp4netns. Unpacking writes over
	// bin/podman while other invocations are running it, and those fail with
	// "fork/exec .../bin/podman: text file busy" - a command that never runs,
	// reported to the caller as nothing at all.
	// The marker is written only after both steps have succeeded, so an unpack
	// that was interrupted is done again.
	if _, err := os.Stat(conf.unpackedMarker()); err == nil {
		return nil
	}

	err = untar(bytes.NewReader(conf.pack), conf.targetDir)
	if err != nil {
		return err
	}

	// setup our custom configs
	err = conf.setupConfs()
	if err != nil {
		return err
	}

	return os.WriteFile(conf.unpackedMarker(), nil, 0o644)
}
