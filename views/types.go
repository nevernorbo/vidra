// Package views renders vidra's pages and htmx fragments, and defines the records they show.
package views

import "database/sql"

type User struct {
	ID         int64
	Name, Role string
}

func (u *User) IsAdmin() bool { return u.Role == "admin" }

type Video struct {
	ID, Title, Visibility, Status      string
	UserID, Size, CreatedAt, ExpiresAt int64
	Width, Height                      int
	By                                 string // uploader's name, set only in the shared-with-me list
}

type Usage struct {
	Used, Quota int64
	Pct         int
	OOB         bool
}

type Invite struct {
	Token             string
	ExpiresAt         int64
	UsedBy            sql.NullString
	Active, IsExpired bool
}

type Member struct {
	ID             int64
	Name, Role, By string
	JoinedAt       int64
}
