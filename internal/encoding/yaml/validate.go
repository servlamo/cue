// Copyright 2025 CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package yaml

import (
	"errors"
	"io"

	"cuelang.org/go/cue"
	"cuelang.org/go/internal/pkg"
)

// Validate validates YAML and confirms it is an instance of schema.
// If the YAML source is a stream, every object must match v.
func Validate(b []byte, v cue.Value) (bool, error) {
	d := NewDecoder("yaml.Validate", b)
	r := v.Context()
	for {
		expr, err := d.Decode()
		if err != nil {
			if err == io.EOF {
				return true, nil
			}
			return false, err
		}

		x := r.BuildExpr(expr)
		if err := x.Err(); err != nil {
			return false, err
		}

		// TODO: consider using subsumption again here.
		// Alternatives:
		// - allow definition of non-concrete list,
		//   like list.Of(int), or []int.
		// - Introduce ! in addition to ?, allowing:
		//   list!: [...]
		// if err := v.Subsume(inst.Value(), cue.Final()); err != nil {
		// 	return false, err
		// }
		x = v.Unify(x)
		if err := x.Err(); err != nil {
			return false, err
		}
		if err := x.Validate(cue.Concrete(true)); err != nil {
			// Strip error codes: incomplete errors are terminal in this case.
			var b pkg.Bottomer
			if errors.As(err, &b) {
				err = b.Bottom().Err
			}
			return false, err
		}
	}
}

// ValidatePartial validates YAML and confirms it matches the constraints
// specified by v using unification. This means that b must be consistent with,
// but does not have to be an instance of v. If the YAML source is a stream,
// every object must match v.
func ValidatePartial(b []byte, v cue.Value) (bool, error) {
	d := NewDecoder("yaml.ValidatePartial", b)
	r := v.Context()
	for {
		expr, err := d.Decode()
		if err != nil {
			if err == io.EOF {
				return true, nil
			}
			return false, err
		}

		x := r.BuildExpr(expr)
		if err := x.Err(); err != nil {
			return false, err
		}

		if x := v.Unify(x); x.Err() != nil {
			return false, x.Err()
		}
	}
}
