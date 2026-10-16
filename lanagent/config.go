// Copyright (c) 2022-2024 The authors (see the AUTHORS file)
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package lanagent

import "time"

// Defaults applied when a period is left at zero.
const (
	DefaultScanPeriod  = 60 * time.Second
	DefaultCheckPeriod = 10 * time.Second
)

// CameraConfig describes a camera named in the configuration rather than
// discovered.
type CameraConfig struct {
	// Address is the device's host, or host:port when its ONVIF service is not
	// on port 80: the form WS-Discovery reports, from which the device service
	// URL http://<Address>/onvif/device_service is built. A URL is refused.
	Address string

	// UUID is the identifier the camera is registered under. Left empty, the
	// device is asked for its endpoint reference, which is the identifier
	// WS-Discovery advertises; a camera that cannot answer that needs one here.
	UUID string

	// User and Password replace the agent's credentials for this camera, each
	// on its own: a field left empty keeps the agent's.
	User     string
	Password string
}

// Config is what the LAN agent needs to run. It carries no serialisation tags
// on purpose: the on-disk format belongs to the binary, which maps its own
// configuration onto this.
type Config struct {
	// User and Password are the ONVIF credentials used to reach the cameras.
	User     string
	Password string

	// Interfaces are always used; DiscoverPatterns select among the interfaces
	// found on the system. Each pattern is a regular expression that has to
	// match the whole interface name, a leading "!" excluding a match. The first
	// pattern that matches decides, and an interface none matches is left out.
	// A pattern written as a glob, "eth*", is refused rather than read as "et"
	// followed by any number of "h".
	Interfaces       []string
	DiscoverPatterns []string

	// Cameras are the statically configured devices, reached in addition to
	// the discovered ones; discovery leaves their addresses to the static loop.
	// They are never purged.
	Cameras []CameraConfig

	ScanPeriod  time.Duration
	CheckPeriod time.Duration

	// GraceGenerations is how many discovery rounds a camera may be missing
	// from before it is forgotten. Zero disables the purge entirely.
	GraceGenerations uint32
}

func (cfg *Config) scanPeriod() time.Duration {
	if cfg.ScanPeriod > 0 {
		return cfg.ScanPeriod
	}
	return DefaultScanPeriod
}

func (cfg *Config) checkPeriod() time.Duration {
	if cfg.CheckPeriod > 0 {
		return cfg.CheckPeriod
	}
	return DefaultCheckPeriod
}
