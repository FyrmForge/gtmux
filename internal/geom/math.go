package geom

// Abs returns the absolute value of an integer.
func Abs(value int) int {
	if value < 0 {
		return -1 * value
	}
	return value
}

// Max returns the largest integer
func Max(first int, second int) int {
	if first >= second {
		return first
	}
	return second
}

// Min returns the smallest integer
func Min(first int, second int) int {
	if first <= second {
		return first
	}
	return second
}

func Clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}

	return value
}
