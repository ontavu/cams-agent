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

import (
	"testing"
	"time"
)

// TestConfig_Defaults pins that an unset period means "use the default" rather
// than "never" -- a zero ticker would leave the hub unaware of every camera.
func TestConfig_Defaults(t *testing.T) {
	cfg := Config{}

	if got := cfg.registerPeriod(); got != DefaultRegisterPeriod {
		t.Fatalf("registerPeriod() = %v, want %v", got, DefaultRegisterPeriod)
	}
	if got := cfg.retryPeriod(); got != DefaultRetryPeriod {
		t.Fatalf("retryPeriod() = %v, want %v", got, DefaultRetryPeriod)
	}
}

func TestConfig_Overrides(t *testing.T) {
	cfg := Config{RegisterPeriod: 3 * time.Second, RetryPeriod: 250 * time.Millisecond}

	if got := cfg.registerPeriod(); got != 3*time.Second {
		t.Fatalf("registerPeriod() = %v, want 3s", got)
	}
	if got := cfg.retryPeriod(); got != 250*time.Millisecond {
		t.Fatalf("retryPeriod() = %v, want 250ms", got)
	}
}

// TestConfig_NegativeFallsBack guards against a configuration file with a
// nonsense value producing a hot loop.
func TestConfig_NegativeFallsBack(t *testing.T) {
	cfg := Config{RegisterPeriod: -time.Second, RetryPeriod: -time.Second}

	if got := cfg.registerPeriod(); got != DefaultRegisterPeriod {
		t.Fatalf("registerPeriod() = %v, want the default", got)
	}
	if got := cfg.retryPeriod(); got != DefaultRetryPeriod {
		t.Fatalf("retryPeriod() = %v, want the default", got)
	}
}
