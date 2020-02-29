package hero

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/kataras/iris/v12/context"
)

type Binding struct {
	Dependency *Dependency
	Input      *Input
}

type Input struct {
	Index            int   // for func inputs
	StructFieldIndex []int // for struct fields in order to support embedded ones.
	Type             reflect.Type

	selfValue reflect.Value // reflect.ValueOf(*Input) cache.
}

func newInput(typ reflect.Type, index int, structFieldIndex []int) *Input {
	in := &Input{
		Index:            index,
		StructFieldIndex: structFieldIndex,
		Type:             typ,
	}

	in.selfValue = reflect.ValueOf(in)
	return in
}

func (b *Binding) String() string {
	index := fmt.Sprintf("%d", b.Input.Index)
	if len(b.Input.StructFieldIndex) > 0 {
		for j, i := range b.Input.StructFieldIndex {
			if j == 0 {
				index = fmt.Sprintf("%d", i)
				continue
			}
			index += fmt.Sprintf(".%d", i)
		}
	}

	return fmt.Sprintf("[%s:%s] maps to [%s]", index, b.Input.Type.String(), b.Dependency)
}

func (b *Binding) Equal(other *Binding) bool {
	if b == nil {
		return other == nil
	}

	if other == nil {
		return false
	}

	// if b.String() != other.String() {
	// 	return false
	// }

	if expected, got := b.Dependency != nil, other.Dependency != nil; expected != got {
		return false
	}

	if expected, got := fmt.Sprintf("%v", b.Dependency.OriginalValue), fmt.Sprintf("%v", other.Dependency.OriginalValue); expected != got {
		return false
	}

	if expected, got := b.Dependency.DestType != nil, other.Dependency.DestType != nil; expected != got {
		return false
	}

	if b.Dependency.DestType != nil {
		if expected, got := b.Dependency.DestType.String(), other.Dependency.DestType.String(); expected != got {
			return false
		}
	}

	if expected, got := b.Input != nil, other.Input != nil; expected != got {
		return false
	}

	if b.Input != nil {
		if expected, got := b.Input.Index, other.Input.Index; expected != got {
			return false
		}

		if expected, got := b.Input.Type.String(), other.Input.Type.String(); expected != got {
			return false
		}

		if expected, got := b.Input.StructFieldIndex, other.Input.StructFieldIndex; !reflect.DeepEqual(expected, got) {
			return false
		}
	}

	return true
}

func matchDependency(dep *Dependency, in reflect.Type) bool {
	if dep.Explicit {
		return dep.DestType == in
	}

	return dep.DestType == nil || equalTypes(dep.DestType, in)
}

func getBindingsFor(inputs []reflect.Type, deps []*Dependency, paramStartIndex int) (bindings []*Binding) {
	bindedInput := make(map[int]struct{})

	// lastParamIndex is used to bind parameters correctly when:
	// otherDep, param1, param2 string and param1 string, otherDep, param2 string.
	lastParamIndex := paramStartIndex
	getParamIndex := func(index int) (paramIndex int) {
		// if len(bindings) > 0 {
		// 	// mostly, it means it's binding to a struct's method, which first value is always the ptr struct as its receiver.
		// 	// so we  decrement the parameter index otherwise first parameter would be declared as parameter index 1 instead of 0.
		// 	paramIndex = len(bindings) + lastParamIndex - 1
		// 	lastParamIndex = paramIndex + 1
		// 	return paramIndex
		// }

		// lastParamIndex = index + 1
		// return index

		paramIndex = lastParamIndex
		lastParamIndex = paramIndex + 1
		return
	}

	for i, in := range inputs { //order matters.

		_, canBePathParameter := context.ParamResolvers[in]
		canBePathParameter = canBePathParameter && paramStartIndex != -1 // if -1 then parameter resolver is disabled.

		prevN := len(bindings) // to check if a new binding is attached; a dependency was matched (see below).

		for j := len(deps) - 1; j >= 0; j-- {
			d := deps[j]
			// Note: we could use the same slice to return.
			//
			// Add all dynamic dependencies (caller-selecting) and the exact typed dependencies.
			//
			// A dependency can only be matched to 1 value, and 1 value has a single dependency
			// (e.g. to avoid conflicting path parameters of the same type).
			if _, alreadyBinded := bindedInput[j]; alreadyBinded {
				continue
			}

			match := matchDependency(d, in)
			if !match {
				continue
			}

			if canBePathParameter {
				// wrap the existing dependency handler.
				paramHandler := paramDependencyHandler(getParamIndex((i)))
				prevHandler := d.Handle
				d.Handle = func(ctx context.Context, input *Input) (reflect.Value, error) {
					v, err := paramHandler(ctx, input)
					if err != nil {
						v, err = prevHandler(ctx, input)
					}

					return v, err
				}
				d.Static = false
				d.OriginalValue = nil
			}

			bindings = append(bindings, &Binding{
				Dependency: d,
				Input:      newInput(in, i, nil),
			})

			if !d.Explicit { // if explicit then it can be binded to more than one input
				bindedInput[j] = struct{}{}
			}

			break
		}

		if prevN == len(bindings) {
			if canBePathParameter {
				// no new dependency added for this input,
				// let's check for path parameters.
				bindings = append(bindings, paramBinding(i, getParamIndex(i), in))
				continue
			}

			// else add builtin bindings that may be registered by user too, but they didn't.
			if indirectType(in).Kind() == reflect.Struct {
				bindings = append(bindings, payloadBinding(i, in))
				continue
			}
		}
	}

	return
}

func getBindingsForFunc(fn reflect.Value, dependencies []*Dependency, paramStartIndex int) []*Binding {
	fnTyp := fn.Type()
	if !isFunc(fnTyp) {
		panic("bindings: unresolved: not a func type")
	}

	n := fnTyp.NumIn()
	inputs := make([]reflect.Type, n)
	for i := 0; i < n; i++ {
		inputs[i] = fnTyp.In(i)
	}

	bindings := getBindingsFor(inputs, dependencies, paramStartIndex)
	if expected, got := n, len(bindings); expected > got {
		panic(fmt.Sprintf("expected [%d] bindings (input parameters) but got [%d]", expected, got))
	}

	return bindings
}

func getBindingsForStruct(v reflect.Value, dependencies []*Dependency, paramStartIndex int, sorter Sorter) (bindings []*Binding) {
	typ := indirectType(v.Type())
	if typ.Kind() != reflect.Struct {
		panic("bindings: unresolved: no struct type")
	}

	// get bindings from any struct's non zero values first, including unexported.
	elem := reflect.Indirect(v)
	nonZero := lookupNonZeroFieldValues(elem)
	for _, f := range nonZero {
		// fmt.Printf("Controller [%s] | NonZero | Field Index: %v | Field Type: %s\n", typ, f.Index, f.Type)
		bindings = append(bindings, &Binding{
			Dependency: NewDependency(elem.FieldByIndex(f.Index).Interface()),
			Input:      newInput(f.Type, f.Index[0], f.Index),
		})
	}

	fields := lookupFields(elem, true, true, nil)
	n := len(fields)

	if n > 1 && sorter != nil {
		sort.Slice(fields, func(i, j int) bool {
			return sorter(fields[i].Type, fields[j].Type)
		})
	}

	inputs := make([]reflect.Type, n)
	for i := 0; i < n; i++ {
		//	fmt.Printf("Controller [%s] | Field Index: %v | Field Type: %s\n", typ, fields[i].Index, fields[i].Type)
		inputs[i] = fields[i].Type
	}
	exportedBindings := getBindingsFor(inputs, dependencies, paramStartIndex)

	// fmt.Printf("Controller [%s] Inputs length: %d vs Bindings length: %d\n", typ, n, len(exportedBindings))
	if len(nonZero) >= len(exportedBindings) { // if all are fields are defined then just return.
		return
	}

	// get declared bindings from deps.
	bindings = append(bindings, exportedBindings...)
	for _, binding := range bindings {
		if len(binding.Input.StructFieldIndex) == 0 {
			// set correctly the input's field index.
			structFieldIndex := fields[binding.Input.Index].Index
			binding.Input.StructFieldIndex = structFieldIndex
		}

		// fmt.Printf("Controller [%s] | Binding Index: %v | Binding Type: %s\n", typ, binding.Input.StructFieldIndex, binding.Input.Type)

		// fmt.Printf("Controller [%s] Set [%s] to struct field index: %v\n", typ.String(), binding.Input.Type.String(), structFieldIndex)
	}

	return
}

/*
	Builtin dynamic bindings.
*/

func paramBinding(index, paramIndex int, typ reflect.Type) *Binding {
	return &Binding{
		Dependency: &Dependency{Handle: paramDependencyHandler(paramIndex), DestType: typ, Source: getSource()},
		Input:      newInput(typ, index, nil),
	}
}

func paramDependencyHandler(paramIndex int) DependencyHandler {
	return func(ctx context.Context, input *Input) (reflect.Value, error) {
		if ctx.Params().Len() <= paramIndex {
			return emptyValue, ErrSeeOther
		}

		return reflect.ValueOf(ctx.Params().Store[paramIndex].ValueRaw), nil
	}
}

// registered if input parameters are more than matched dependencies.
// It binds an input to a request body based on the request content-type header (JSON, XML, YAML, Query, Form).
func payloadBinding(index int, typ reflect.Type) *Binding {
	return &Binding{
		Dependency: &Dependency{
			Handle: func(ctx context.Context, input *Input) (newValue reflect.Value, err error) {
				wasPtr := input.Type.Kind() == reflect.Ptr

				newValue = reflect.New(indirectType(input.Type))
				ptr := newValue.Interface()

				switch ctx.GetContentTypeRequested() {
				case context.ContentXMLHeaderValue:
					err = ctx.ReadXML(ptr)
				case context.ContentYAMLHeaderValue:
					err = ctx.ReadYAML(ptr)
				case context.ContentFormHeaderValue:
					err = ctx.ReadQuery(ptr)
				case context.ContentFormMultipartHeaderValue:
					err = ctx.ReadForm(ptr)
				default:
					err = ctx.ReadJSON(ptr)
					// json
				}

				// if err != nil {
				// 	return emptyValue, err
				// }

				if !wasPtr {
					newValue = newValue.Elem()
				}

				return
			},
			Source: getSource(),
		},
		Input: newInput(typ, index, nil),
	}

}
