package fixture

import wall "time"

func aliased() wall.Time {
	return wall.Time{}
}

var _ = wall.Now
