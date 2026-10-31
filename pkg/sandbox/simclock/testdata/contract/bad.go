package fixture

import "time"

type T struct{ now func() time.Time }

var viaRef = T{now: time.Now}

func call() time.Time {

	return time.Now()
}

func since(t time.Time) time.Duration {
	return time.Since(t)
}

func wait() <-chan time.Time {
	return time.After(time.Second)
}

func tick() {
	_ = time.NewTicker(time.Second)
}
