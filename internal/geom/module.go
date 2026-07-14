package geom

type Vec2 struct {
	// row (y)
	R int
	// column (x)
	C int
}

func (v Vec2) Add(other Vec2) Vec2 {
	return Vec2{
		R: other.R + v.R,
		C: other.C + v.C,
	}
}

type Size = Vec2

type Rect struct {
	Position, Size Vec2
}

func (r Rect) Contains(v Vec2) bool {
	return v.R >= r.Position.R && v.R < r.Position.R+r.Size.R &&
		v.C >= r.Position.C &&
		v.C < r.Position.C+r.Size.C
}

var DEFAULT_SIZE = Vec2{
	R: 24,
	C: 80,
}
