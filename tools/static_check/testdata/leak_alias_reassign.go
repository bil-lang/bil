// A channel copied via plain `=` reassignment (not `:=`) — leak_alias.go
// only exercises the `:=` shape; CheckChannelConfinement's AssignStmt case
// doesn't discriminate on token, but this locks in the `=` shape
// explicitly, matching bilc's own retired checkNoChannelAliasing fixture
// (alias-assign.bil, since removed). Must be
// flagged (LeakAlias).
package main

func main() {
	c := make(chan int)
	var d chan int
	d = c
	d <- 1
}
