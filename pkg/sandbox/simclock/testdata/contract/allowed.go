package fixture

import "time"

func sameLine() time.Time {
	return time.Now() //simclock:wall an ID generator
}

func lineAbove() time.Time {
	//simclock:wall a stop wait
	return time.Now()
}

func durationsAreFine() time.Duration { return time.Second + time.Duration(3) }
