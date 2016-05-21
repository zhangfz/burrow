// Copyright 2015, 2016 Eris Industries (UK) Ltd.
// This file is part of Eris-RT

// Eris-RT is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Eris-RT is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with Eris-RT.  If not, see <http://www.gnu.org/licenses/>.

package version

import (
  "fmt"
)

const VERSION = "0.12.0"
const TENDERMINT_VERSION = "0.6.0"

const (
  // Client identifier to advertise over the network
  clientIdentifier = "eris-db"
  // Major version component of the current release
  versionMajor     = 0
  // Minor version component of the current release
  versionMinor     = 12
  // Patch version component of the current release
  versionPatch     = 0
)

func GetVersionString() string {
  return fmt.Sprintf("%s-%d.%d.%d", clientIdentifier, versionMajor, versionMinor,
    versionPatch)
}