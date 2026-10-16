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
	Address  string
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
	// found on the system, a leading "!" excluding a match.
	Interfaces       []string
	DiscoverPatterns []string

	// Cameras are the statically configured devices.
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
