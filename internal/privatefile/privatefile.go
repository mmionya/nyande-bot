// Package privatefile creates private storage files using native permissions.
package privatefile

import "errors"

var ErrPermissions = errors.New("storage permissions allow access by another account")
