package terraform

import (
	"fmt"
	"log"
	"math/big"
	"sort"
	"strings"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/configs/configschema"
	"github.com/hashicorp/terraform/providers"
	"github.com/hashicorp/terraform/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

// ImportStateTransformer is a GraphTransformer that adds nodes to the
// graph to represent the imports we want to do for resources.
type ImportStateTransformer struct {
	Targets []*ImportTarget
	Config  *configs.Config
}

func (t *ImportStateTransformer) Transform(g *Graph) error {
	for _, target := range t.Targets {

		// This is only likely to happen in misconfigured tests
		if t.Config == nil {
			return fmt.Errorf("Cannot import into an empty configuration.")
		}

		// Get the module config
		modCfg := t.Config.Descendent(target.Addr.Module.Module())
		if modCfg == nil {
			return fmt.Errorf("Module %s not found.", target.Addr.Module.Module())
		}

		providerAddr := addrs.AbsProviderConfig{
			Module: target.Addr.Module.Module(),
		}

		// Try to find the resource config
		rsCfg := modCfg.Module.ResourceByAddr(target.Addr.Resource.Resource)
		if rsCfg != nil {
			// Get the provider FQN for the resource from the resource configuration
			providerAddr.Provider = rsCfg.Provider

			// Get the alias from the resource's provider local config
			providerAddr.Alias = rsCfg.ProviderConfigAddr().Alias
		} else {
			// Resource has no matching config, so use an implied provider
			// based on the resource type
			rsProviderType := target.Addr.Resource.Resource.ImpliedProvider()
			providerAddr.Provider = modCfg.Module.ImpliedProviderForUnqualifiedType(rsProviderType)
		}

		node := &graphNodeImportState{
			Addr:         target.Addr,
			ID:           target.ID,
			ProviderAddr: providerAddr,
		}
		g.Add(node)
	}
	return nil
}

type graphNodeImportState struct {
	Addr             addrs.AbsResourceInstance // Addr is the resource address to import into
	ID               string                    // ID is the ID to import as
	ProviderAddr     addrs.AbsProviderConfig   // Provider address given by the user, or implied by the resource type
	ResolvedProvider addrs.AbsProviderConfig   // provider node address after resolution

	states []providers.ImportedResource
}

var (
	_ GraphNodeModulePath        = (*graphNodeImportState)(nil)
	_ GraphNodeExecutable        = (*graphNodeImportState)(nil)
	_ GraphNodeProviderConsumer  = (*graphNodeImportState)(nil)
	_ GraphNodeDynamicExpandable = (*graphNodeImportState)(nil)
)

func (n *graphNodeImportState) Name() string {
	return fmt.Sprintf("%s (import id %q)", n.Addr, n.ID)
}

// GraphNodeProviderConsumer
func (n *graphNodeImportState) ProvidedBy() (addrs.ProviderConfig, bool) {
	// We assume that n.ProviderAddr has been properly populated here.
	// It's the responsibility of the code creating a graphNodeImportState
	// to populate this, possibly by calling DefaultProviderConfig() on the
	// resource address to infer an implied provider from the resource type
	// name.
	return n.ProviderAddr, false
}

// GraphNodeProviderConsumer
func (n *graphNodeImportState) Provider() addrs.Provider {
	// We assume that n.ProviderAddr has been properly populated here.
	// It's the responsibility of the code creating a graphNodeImportState
	// to populate this, possibly by calling DefaultProviderConfig() on the
	// resource address to infer an implied provider from the resource type
	// name.
	return n.ProviderAddr.Provider
}

// GraphNodeProviderConsumer
func (n *graphNodeImportState) SetProvider(addr addrs.AbsProviderConfig) {
	n.ResolvedProvider = addr
}

// GraphNodeModuleInstance
func (n *graphNodeImportState) Path() addrs.ModuleInstance {
	return n.Addr.Module
}

// GraphNodeModulePath
func (n *graphNodeImportState) ModulePath() addrs.Module {
	return n.Addr.Module.Module()
}

// GraphNodeExecutable impl.
func (n *graphNodeImportState) Execute(ctx EvalContext, op walkOperation) (diags tfdiags.Diagnostics) {
	// Reset our states
	n.states = nil

	provider, _, err := GetProvider(ctx, n.ResolvedProvider)
	diags = diags.Append(err)
	if diags.HasErrors() {
		return diags
	}

	// import state
	absAddr := n.Addr.Resource.Absolute(ctx.Path())

	// Call pre-import hook
	diags = diags.Append(ctx.Hook(func(h Hook) (HookAction, error) {
		return h.PreImportState(absAddr, n.ID)
	}))
	if diags.HasErrors() {
		return diags
	}

	resp := provider.ImportResourceState(providers.ImportResourceStateRequest{
		TypeName: n.Addr.Resource.Resource.Type,
		ID:       n.ID,
	})
	diags = diags.Append(resp.Diagnostics)
	if diags.HasErrors() {
		return diags
	}

	imported := resp.ImportedResources
	for _, obj := range imported {
		log.Printf("[TRACE] graphNodeImportState: import %s %q produced instance object of type %s", absAddr.String(), n.ID, obj.TypeName)
	}
	n.states = imported

	// Call post-import hook
	diags = diags.Append(ctx.Hook(func(h Hook) (HookAction, error) {
		return h.PostImportState(absAddr, imported)
	}))
	return diags
}

// GraphNodeDynamicExpandable impl.
//
// We use DynamicExpand as a way to generate the subgraph of refreshes
// and state inserts we need to do for our import state. Since they're new
// resources they don't depend on anything else and refreshes are isolated
// so this is nearly a perfect use case for dynamic expand.
func (n *graphNodeImportState) DynamicExpand(ctx EvalContext) (*Graph, error) {
	var diags tfdiags.Diagnostics

	g := &Graph{Path: ctx.Path()}

	// nameCounter is used to de-dup names in the state.
	nameCounter := make(map[string]int)

	// Compile the list of addresses that we'll be inserting into the state.
	// We do this ahead of time so we can verify that we aren't importing
	// something that already exists.
	addrs := make([]addrs.AbsResourceInstance, len(n.states))
	for i, state := range n.states {
		addr := n.Addr
		if t := state.TypeName; t != "" {
			addr.Resource.Resource.Type = t
		}

		// Determine if we need to suffix the name to de-dup
		key := addr.String()
		count, ok := nameCounter[key]
		if ok {
			count++
			addr.Resource.Resource.Name += fmt.Sprintf("-%d", count)
		}
		nameCounter[key] = count

		// Add it to our list
		addrs[i] = addr
	}

	// Verify that all the addresses are clear
	state := ctx.State()
	for _, addr := range addrs {
		existing := state.ResourceInstance(addr)
		if existing != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Resource already managed by Terraform",
				fmt.Sprintf("Terraform is already managing a remote object for %s. To import to this address you must first remove the existing object from the state.", addr),
			))
			continue
		}
	}
	if diags.HasErrors() {
		// Bail out early, then.
		return nil, diags.Err()
	}

	// For each of the states, we add a node to handle the refresh/add to state.
	// "n.states" is populated by our own Execute with the result of
	// ImportState. Since DynamicExpand is always called after Execute, this is
	// safe.
	for i, state := range n.states {
		g.Add(&graphNodeImportStateSub{
			TargetAddr:       addrs[i],
			State:            state,
			ResolvedProvider: n.ResolvedProvider,
		})
	}

	// Root transform for a single root
	t := &RootTransformer{}
	if err := t.Transform(g); err != nil {
		return nil, err
	}

	// Done!
	return g, diags.Err()
}

// graphNodeImportStateSub is the sub-node of graphNodeImportState
// and is part of the subgraph. This node is responsible for refreshing
// and adding a resource to the state once it is imported.
type graphNodeImportStateSub struct {
	TargetAddr       addrs.AbsResourceInstance
	State            providers.ImportedResource
	ResolvedProvider addrs.AbsProviderConfig
}

var (
	_ GraphNodeModuleInstance = (*graphNodeImportStateSub)(nil)
	_ GraphNodeExecutable     = (*graphNodeImportStateSub)(nil)
)

func (n *graphNodeImportStateSub) Name() string {
	return fmt.Sprintf("import %s result", n.TargetAddr)
}

func (n *graphNodeImportStateSub) Path() addrs.ModuleInstance {
	return n.TargetAddr.Module
}

// GraphNodeExecutable impl.
func (n *graphNodeImportStateSub) Execute(ctx EvalContext, op walkOperation) (diags tfdiags.Diagnostics) {
	// If the Ephemeral type isn't set, then it is an error
	if n.State.TypeName == "" {
		diags = diags.Append(fmt.Errorf("import of %s didn't set type", n.TargetAddr.String()))
		return diags
	}

	state := n.State.AsInstanceObject()
	provider, providerSchema, err := GetProvider(ctx, n.ResolvedProvider)
	diags = diags.Append(err)
	if diags.HasErrors() {
		return diags
	}

	// EvalRefresh
	evalRefresh := &EvalRefresh{
		Addr:           n.TargetAddr.Resource,
		ProviderAddr:   n.ResolvedProvider,
		Provider:       &provider,
		ProviderSchema: &providerSchema,
		State:          &state,
		Output:         &state,
	}
	diags = diags.Append(evalRefresh.Eval(ctx))
	if diags.HasErrors() {
		return diags
	}

	// Verify the existance of the imported resource
	if state.Value.IsNull() {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Cannot import non-existent remote object",
			fmt.Sprintf(
				"While attempting to import an existing object to %s, the provider detected that no object exists with the given id. Only pre-existing objects can be imported; check that the id is correct and that it is associated with the provider's configured region or endpoint, or use \"terraform apply\" to create a new remote object for this resource.",
				n.TargetAddr.Resource.String(),
			),
		))
		return diags
	}

	//EvalWriteState
	evalWriteState := &EvalWriteState{
		Addr:           n.TargetAddr.Resource,
		ProviderAddr:   n.ResolvedProvider,
		ProviderSchema: &providerSchema,
		State:          &state,
	}
	diags = diags.Append(evalWriteState.Eval(ctx))

	// Print out the config block for this resource
	fmt.Println("Importing the following resource into Terraform...")
	res := n.TargetAddr.Resource.ContainingResource()
	fmt.Printf("resource \"%s\" \"%s\" {\n", res.Type, res.Name)
	schema, _ := providerSchema.SchemaForResourceAddr(res)
	printConfig(schema, state.Value, "")
	fmt.Println("}")
	return diags
}

func printConfig(schema *configschema.Block, state cty.Value, blockName string) {
	// Sort the attribute names
	var attrNames []string
	spaces := 2
	for name, attrS := range schema.Attributes {
		// Skip the id field
		if blockName == "" && name == "id" {
			continue
		}
		v := state.GetAttr(name)
		// Skip null values & compute-only values
		isWhollyComputed := attrS.Computed && !attrS.Optional
		if v.IsNull() || isWhollyComputed {
			continue
		}

		attrNames = append(attrNames, name)
	}
	if len(attrNames) == 0 {
		return
	}
	// Print the block if needed
	if blockName != "" {
		fmt.Printf("%s%s {\n", strings.Repeat(" ", spaces), blockName)
		spaces = 4
	}
	sort.Strings(attrNames)

	for _, name := range attrNames {
		v := state.GetAttr(name)
		// fmt.Println(stringValueForImport(val))
		sVal, ok := stringValueForImport(v)
		if ok {
			fmt.Printf("%s%s = %s\n", strings.Repeat(" ", spaces), name, sVal)
		}
	}

	if blockName != "" {
		fmt.Println("  }")
	}

	for name, blockS := range schema.BlockTypes {
		blockV := state.GetAttr(name)
		switch blockS.Nesting {
		case configschema.NestingSingle, configschema.NestingGroup:
			printConfig(&blockS.Block, blockV, name)
		case configschema.NestingList, configschema.NestingMap, configschema.NestingSet:
			for it := blockV.ElementIterator(); it.Next(); {
				_, blockEV := it.Element()
				printConfig(&blockS.Block, blockEV, name)
			}
		default:
			panic(fmt.Sprintf("unsupported nesting mode %s", blockS.Nesting))
		}
	}
}

func stringValueForImport(val cty.Value) (string, bool) {
	var s string
	ty := val.Type()

	switch {
	case ty.IsPrimitiveType():
		switch ty {
		case cty.String:
			vS := val.AsString()
			return fmt.Sprintf("\"%s\"", vS), vS != ""
		case cty.Bool:
			if val.True() {
				return "true", true
			}
			return "false", true
		case cty.Number:
			bf := val.AsBigFloat()
			return bf.Text('f', -1), bf != big.NewFloat(0)
		}
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		if val.LengthInt() == 0 {
			return s, false
		}
		s = "[\n"

		it := val.ElementIterator()
		for it.Next() {
			_, val := it.Element()
			sV, ok := stringValueForImport(val)
			if ok {
				s = s + "    " + sV + ",\n"
			}
		}
		s = s + "  ]"
		return s, true
	case ty.IsMapType():
		if val.RawEquals(cty.MapValEmpty(ty.ElementType())) {
			return s, false
		}
		return fmt.Sprintf("%#v", val), true
	default:
		return fmt.Sprintf("%#v", val), true
	}
	return s, true
}
