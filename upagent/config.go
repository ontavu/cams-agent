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

package upagent

import "time"

// Defaults applied when a period is left at zero.
const (
	DefaultRegisterPeriod = 30 * time.Second

	// DefaultRetryPeriod is how long to wait between two attempts at the hub.
	// It exists to keep a refused connection from becoming a busy loop.
	DefaultRetryPeriod = time.Second

	// DefaultRequestTimeout bounds one unary call to the hub.
	DefaultRequestTimeout = 30 * time.Second
)

// Config is what the upstream agent needs to run. It carries no serialisation
// tags on purpose: the on-disk format belongs to the binary, which maps its own
// configuration onto this.
type Config struct {
	// User identifies the account the local streams belong to. It travels as
	// gRPC metadata on every call, and inside each registration.
	User string

	// ControlAddress is the hub's control plane, as host:port.
	ControlAddress string

	// RegisterPeriod is how often the local streams are announced again.
	RegisterPeriod time.Duration

	// RetryPeriod is how long to pause between two connection attempts.
	RetryPeriod time.Duration

	// RequestTimeout bounds one unary call to the hub, so that a hub which
	// accepts TCP and then says nothing cannot park the registration loop.
	//
	// It deliberately does not apply to the control stream or to a media
	// upload: those are long-lived by design, and a deadline on them would cut
	// a healthy connection. Their liveness comes from the gRPC keepalive
	// wire.DialOptions passes to utils.DialInsecure.
	RequestTimeout time.Duration
}

func (cfg *Config) registerPeriod() time.Duration {
	if cfg.RegisterPeriod > 0 {
		return cfg.RegisterPeriod
	}
	return DefaultRegisterPeriod
}

func (cfg *Config) retryPeriod() time.Duration {
	if cfg.RetryPeriod > 0 {
		return cfg.RetryPeriod
	}
	return DefaultRetryPeriod
}

func (cfg *Config) requestTimeout() time.Duration {
	if cfg.RequestTimeout > 0 {
		return cfg.RequestTimeout
	}
	return DefaultRequestTimeout
}
