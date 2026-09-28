// Package sqlite stands in for github.com/glebarez/go-sqlite, a fork of
// modernc.org/sqlite that registers itself under the same "sqlite" driver
// name. CasOS links modernc.org/sqlite through kine and its own ORM, and the
// embedded mesh coordination server links the fork through gorm, so the two
// registrations would panic at startup. This package hands gorm the modernc
// driver instead: it registers "sqlite" and has the same Error type.
package sqlite

import "modernc.org/sqlite"

type Error = sqlite.Error
