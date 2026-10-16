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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

var encoded = `{
    "discover": ["eth*", "enp*", "!wl*", "!lo"],
    "scan_period": 5,
    "check_period": 5,
    "register_period": 5,
    "interfaces": [ "eno0" ],
    "cameras": [
        {"address": "127.0.0.1", "user":"admin" },
        {"address": "127.0.0.2", "user":"admin" }
    ],
    "control": {"address": "127.0.0.1:6000", "timeout": 10},
    "media": {"address": "127.0.0.1:6000", "timeout": 10}
}`

func assertValue[T comparable](t *testing.T, decoded, expected T) {
	if expected != decoded {
		t.Fatal("diff", expected, decoded)
	}
}

func assertArrays[T comparable](t *testing.T, decoded, expected []T) {
	if len(expected) != len(decoded) {
		t.Fatal(len(expected), len(decoded))
	}
	for i := 0; i < len(expected); i++ {
		assertValue(t, expected[i], decoded[i])
	}
}

func assertConfig(t *testing.T, decoded, expected AgentConfig) {
	assertValue(t, decoded.ScanPeriod, expected.ScanPeriod)
	assertValue(t, decoded.CheckPeriod, expected.CheckPeriod)
	assertArrays(t, decoded.DiscoverPatterns, expected.DiscoverPatterns)
	assertValue(t, decoded.UpstreamControl, expected.UpstreamControl)
	assertValue(t, decoded.UpstreamMedia, expected.UpstreamMedia)
	assertArrays(t, decoded.Interfaces, expected.Interfaces)
	assertArrays(t, decoded.Cameras, expected.Cameras)
}

func TestConfig_FromEmpty(t *testing.T) {
	var cfg AgentConfig
	if err := cfg.LoadString(encoded); err != nil {
		t.Fatal(err)
	}
	assertConfig(t, cfg, AgentConfig{
		DiscoverPatterns: []string{"eth*", "enp*", "!wl*", "!lo"},
		ScanPeriod:       5,
		CheckPeriod:      5,
		RegisterPeriod:   5,
		Interfaces:       []string{"eno0"},
		Cameras: []CameraConfig{
			{Address: "127.0.0.1", User: "admin"},
			{Address: "127.0.0.2", User: "admin"},
		},
		UpstreamControl: UpstreamConfig{Address: "127.0.0.1:6000", Timeout: 10},
		UpstreamMedia:   UpstreamConfig{Address: "127.0.0.1:6000", Timeout: 10},
	})
}

// TestConfig_Override validates that the arrays are not appended when values
// are present in the encoded form
func TestConfig_Override(t *testing.T) {
	cfg := AgentConfig{
		DiscoverPatterns: []string{"x", "y"},
		ScanPeriod:       17,
		CheckPeriod:      18,
		RegisterPeriod:   19,
		Interfaces:       []string{"xxxx"},
		Cameras: []CameraConfig{
			{Address: "127.0.0.1", User: "x"},
			{Address: "127.0.0.2", User: "y"},
			{Address: "127.0.0.2", User: "Z"},
		},
		UpstreamControl: UpstreamConfig{Address: "127.0.0.1:6001", Timeout: 10},
		UpstreamMedia:   UpstreamConfig{Address: "127.0.0.1:6001", Timeout: 10},
	}
	if err := cfg.LoadString(encoded); err != nil {
		t.Fatal(err)
	}

	assertConfig(t, cfg, AgentConfig{
		DiscoverPatterns: []string{"eth*", "enp*", "!wl*", "!lo"},
		ScanPeriod:       5,
		CheckPeriod:      5,
		RegisterPeriod:   5,
		Interfaces:       []string{"eno0"},
		Cameras: []CameraConfig{
			{Address: "127.0.0.1", User: "admin"},
			{Address: "127.0.0.2", User: "admin"},
		},
		UpstreamControl: UpstreamConfig{Address: "127.0.0.1:6000", Timeout: 10},
		UpstreamMedia:   UpstreamConfig{Address: "127.0.0.1:6000", Timeout: 10},
	})
}

// The two upstreams are different processes on different ports, and the
// defaults used to name the same one -- which sent every media upload to
// cams-ctrl, whose MediaUpload answers Unimplemented. Nothing else in this file
// covers DefaultConfig, because every other case loads an explicit fixture,
// which is how that survived.
func TestConfig_DefaultsSeparateControlFromMedia(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.UpstreamControl.Address != DefaultControlAddr {
		t.Fatal("control", cfg.UpstreamControl.Address, "want", DefaultControlAddr)
	}
	if cfg.UpstreamMedia.Address != DefaultMediaAddr {
		t.Fatal("media", cfg.UpstreamMedia.Address, "want", DefaultMediaAddr)
	}
	if cfg.UpstreamMedia.Address == cfg.UpstreamControl.Address {
		t.Fatal("the media upload points at the control plane, which refuses it")
	}
}

// The order the four sources resolve in, which is the whole contract of
// loadConfig: defaults, then the file, then the environment, then the flags.
func TestLoadConfig_PrecedenceIsDefaultsFileEnvFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{"user":"from-file","camera_user":"from-file","camera_password":"from-file",
	          "control":{"address":"127.0.0.1:7000"},"media":{"address":"127.0.0.1:7001"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	t.Setenv(EnvCamUser, "from-env")
	t.Setenv(EnvCamPassword, "from-env")

	cfg, err := loadConfig(path, "from-flag", "127.0.0.1:8000", "")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	// The flag beats the file.
	if cfg.User != "from-flag" {
		t.Errorf("user is %q, the flag said from-flag", cfg.User)
	}
	if cfg.UpstreamControl.Address != "127.0.0.1:8000" {
		t.Errorf("control is %q, the flag said 127.0.0.1:8000", cfg.UpstreamControl.Address)
	}
	// The environment beats the file, for the two it carries and no others.
	if cfg.CameraUser != "from-env" || cfg.CameraPassword != "from-env" {
		t.Errorf("credentials are %q/%q, the environment said from-env",
			cfg.CameraUser, cfg.CameraPassword)
	}
	// The file beats the defaults where nothing above it spoke.
	if cfg.UpstreamMedia.Address != "127.0.0.1:7001" {
		t.Errorf("media is %q, the file said 127.0.0.1:7001", cfg.UpstreamMedia.Address)
	}
}

// An unset variable changes nothing. Otherwise a configuration file would stop
// being a complete answer on a machine where nobody exports anything, which is
// most of them.
func TestLoadConfig_TheEnvironmentOnlySpeaksWhenItIsSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path,
		[]byte(`{"camera_user":"from-file","camera_password":"from-file"}`), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	t.Setenv(EnvCamUser, "")
	t.Setenv(EnvCamPassword, "")

	cfg, err := loadConfig(path, "", "", "")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.CameraUser != "from-file" || cfg.CameraPassword != "from-file" {
		t.Errorf("credentials are %q/%q; an empty variable overrode the file",
			cfg.CameraUser, cfg.CameraPassword)
	}
}

// The two upstreams are two processes on two ports. cams-ctrl answers
// MediaUpload with Unimplemented on purpose, so an agent pointed at one address
// for both uploads nothing and reports nothing about why -- which is a failure
// worth refusing at start-up rather than diagnosing from a silent camera.
func TestLoadConfig_RefusesOneAddressForBothUpstreams(t *testing.T) {
	if _, err := loadConfig("", "", "127.0.0.1:6000", "127.0.0.1:6000"); err == nil {
		t.Fatal("one address for control and media was accepted")
	}

	// And the defaults are not that mistake.
	if _, err := loadConfig("", "", "", ""); err != nil {
		t.Fatalf("the defaults are refused: %v", err)
	}
}

// A path that is not there is a failure to start, not a silent fall back to the
// defaults: somebody who passed --config meant the file to be read.
func TestLoadConfig_RefusesAMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent.json"), "", "", ""); err == nil {
		t.Fatal("a missing configuration file was accepted")
	}
}
