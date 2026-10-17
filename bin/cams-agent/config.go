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
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/ontavu/cams-agent/lanagent"
	"github.com/ontavu/cams-agent/upagent"
)

const (
	DefaultScanPeriod      = 60
	DefaultRegisterPeriod  = 5
	DefaultCheckPeriod     = 10
	DefaultUpstreamTimeout = 30

	// The hub has two endpoints, and an agent talks to both.
	//
	// DefaultControlAddr serves Registrar and Controller: the registrations and
	// the Play/Stop commands. DefaultMediaAddr serves Uploader, which carries the
	// RTP upload. They are not interchangeable: the control endpoint answers
	// MediaUpload with Unimplemented, so pointing the media upload at it uploads
	// nothing at all.
	DefaultControlAddr = "127.0.0.1:6000"
	DefaultMediaAddr   = "127.0.0.1:6001"
)

// UpstreamConfig is the hub's control endpoint. Timeout, in seconds, bounds
// one registration call; the control stream itself has none, being long-lived
// by design.
type UpstreamConfig struct {
	Address string `json:"address"`
	Timeout int64  `json:"timeout"`
}

// MediaConfig is the hub's media endpoint. It has no timeout, unlike the
// control one: an upload is a stream that lasts as long as the camera plays,
// and its liveness comes from the gRPC keepalive. It used to share
// UpstreamConfig, so "media.timeout" was accepted and read by nothing; a file
// that still carries it loads, and the field is ignored as before.
type MediaConfig struct {
	Address string `json:"address"`
}

// CameraConfig is one camera the configuration names; lanagent.CameraConfig
// says what each field means.
type CameraConfig struct {
	Address  string `json:"address"`
	UUID     string `json:"uuid,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

type AgentConfig struct {
	User string `json:"user"`

	// CameraUser and CameraPassword are the ONVIF credentials of the local
	// devices. They used to be package-level variables in globals.go.
	CameraUser     string `json:"camera_user"`
	CameraPassword string `json:"camera_password"`

	// GraceGenerations is how many discovery rounds a camera may be missing
	// from before it is forgotten. Zero disables the purge.
	GraceGenerations uint32 `json:"grace_generations"`

	DiscoverPatterns []string `json:"discover"`
	ScanPeriod       int64    `json:"scan_period"`
	CheckPeriod      int64    `json:"check_period"`
	RegisterPeriod   int64    `json:"register_period"`

	Interfaces []string       `json:"interfaces"`
	Cameras    []CameraConfig `json:"cameras"`

	UpstreamControl UpstreamConfig `json:"control"`
	UpstreamMedia   MediaConfig    `json:"media"`
}

func DefaultConfig() AgentConfig {
	return AgentConfig{
		User:             "plop",
		CameraUser:       "admin",
		DiscoverPatterns: []string{"!lo", "!docker.*", ".*"},
		ScanPeriod:       DefaultScanPeriod,
		CheckPeriod:      DefaultCheckPeriod,
		RegisterPeriod:   DefaultRegisterPeriod,
		UpstreamControl:  UpstreamConfig{Address: DefaultControlAddr, Timeout: DefaultUpstreamTimeout},
		UpstreamMedia:    MediaConfig{Address: DefaultMediaAddr},
	}
}

func (cfg *AgentConfig) LoadFile(path string) error {
	if fin, err := os.Open(path); err != nil {
		return fmt.Errorf("open: %w", err)
	} else {
		defer fin.Close()
		if encoded, err := ioutil.ReadAll(fin); err != nil {
			return fmt.Errorf("read: %w", err)
		} else {
			return cfg.LoadBytes(encoded)
		}
	}
}

func (cfg *AgentConfig) LoadBytes(encoded []byte) error {
	if err := json.NewDecoder(bytes.NewReader(encoded)).Decode(cfg); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func (cfg *AgentConfig) LoadString(encoded string) error {
	if err := json.NewDecoder(strings.NewReader(encoded)).Decode(cfg); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// Where the ONVIF credentials come from, with the same two names cams-cli
// takes them under: there is one vocabulary for this on the wire and there
// should be one here.
//
// The environment rather than a flag, because a password on a command line is
// visible in ps to every user of the machine and lands in the shell history;
// and rather than the configuration file alone, because a deployment hands a
// container its secrets as environment and should not have to write a file to
// do it. There is no default password and there must not be one -- a
// compiled-in credential is published with the source and stays in its history,
// where deleting the line does not reach.
const (
	EnvCamUser     = "CAMS_ONVIF_USER"
	EnvCamPassword = "CAMS_ONVIF_PASSWORD"
)

// LoadEnv applies what the environment says, over whatever is already there.
//
// Only the two credentials: everything else a deployment varies is either a
// flag or a line in the configuration file, and both of those are visible in a
// way an environment variable is not. An unset variable changes nothing, so
// that a configuration file remains a complete answer on a machine where
// nobody exports anything.
func (cfg *AgentConfig) LoadEnv() {
	if user, set := os.LookupEnv(EnvCamUser); set && user != "" {
		cfg.CameraUser = user
	}
	if password, set := os.LookupEnv(EnvCamPassword); set && password != "" {
		cfg.CameraPassword = password
	}
}

// lan projects the configuration onto what the LAN agent needs. The agent has
// its own struct, without serialisation tags, so the on-disk format stays here.
func (cfg *AgentConfig) lan() lanagent.Config {
	cameras := make([]lanagent.CameraConfig, 0, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		cameras = append(cameras, lanagent.CameraConfig{
			Address:  cam.Address,
			UUID:     cam.UUID,
			User:     cam.User,
			Password: cam.Password,
		})
	}

	return lanagent.Config{
		User:             cfg.CameraUser,
		Password:         cfg.CameraPassword,
		Interfaces:       cfg.Interfaces,
		DiscoverPatterns: cfg.DiscoverPatterns,
		Cameras:          cameras,
		ScanPeriod:       cfg.GetScanPeriod(),
		CheckPeriod:      cfg.GetCheckPeriod(),
		GraceGenerations: cfg.GraceGenerations,
	}
}

// upstream projects the configuration onto what the upstream agent needs.
func (cfg *AgentConfig) upstream() upagent.Config {
	return upagent.Config{
		User:           cfg.User,
		ControlAddress: cfg.UpstreamControl.Address,
		RegisterPeriod: time.Duration(cfg.RegisterPeriod) * time.Second,
		RequestTimeout: time.Duration(cfg.UpstreamControl.Timeout) * time.Second,
	}
}

func (cfg *AgentConfig) GetScanPeriod() time.Duration {
	return time.Duration(cfg.ScanPeriod) * time.Second
}

func (cfg *AgentConfig) GetCheckPeriod() time.Duration {
	return time.Duration(cfg.CheckPeriod) * time.Second
}
