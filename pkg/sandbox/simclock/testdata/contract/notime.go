package fixture

type clock struct{}

func (clock) Now() int { return 0 }

func notTheTimePackage(c clock) int { return c.Now() }
