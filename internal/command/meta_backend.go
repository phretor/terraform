package command

// This file contains all the Backend-related function calls on Meta,
// exported and private.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/terraform/internal/backend"
	remoteBackend "github.com/hashicorp/terraform/internal/backend/remote"
	"github.com/hashicorp/terraform/internal/command/arguments"
	"github.com/hashicorp/terraform/internal/command/clistate"
	"github.com/hashicorp/terraform/internal/command/views"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/states/statemgr"
	"github.com/hashicorp/terraform/internal/terraform"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	backendInit "github.com/hashicorp/terraform/internal/backend/init"
	backendLocal "github.com/hashicorp/terraform/internal/backend/local"
	legacy "github.com/hashicorp/terraform/internal/legacy/terraform"
)

// BackendOpts are the options used to initialize a backend.Backend.
type BackendOpts struct {
	// Config is a representation of the backend configuration block given in
	// the root module, or nil if no such block is present.
	Config *configs.Backend

	// ConfigOverride is an hcl.Body that, if non-nil, will be used with
	// configs.MergeBodies to override the type-specific backend configuration
	// arguments in Config.
	ConfigOverride hcl.Body

	// Init should be set to true if initialization is allowed. If this is
	// false, then any configuration that requires configuration will show
	// an error asking the user to reinitialize.
	Init bool

	// ForceLocal will force a purely local backend, including state.
	// You probably don't want to set this.
	ForceLocal bool
}

// Backend initializes and returns the backend for this CLI session.
//
// The backend is used to perform the actual Terraform operations. This
// abstraction enables easily sliding in new Terraform behavior such as
// remote state storage, remote operations, etc. while allowing the CLI
// to remain mostly identical.
//
// This will initialize a new backend for each call, which can carry some
// overhead with it. Please reuse the returned value for optimal behavior.
//
// Only one backend should be used per Meta. This function is stateful
// and is unsafe to create multiple backends used at once. This function
// can be called multiple times with each backend being "live" (usable)
// one at a time.
//
// A side-effect of this method is the population of m.backendState, recording
// the final resolved backend configuration after dealing with overrides from
// the "terraform init" command line, etc.
func (m *Meta) Backend(opts *BackendOpts) (backend.Enhanced, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// If no opts are set, then initialize
	if opts == nil {
		opts = &BackendOpts{}
	}

	// Initialize a backend from the config unless we're forcing a purely
	// local operation.
	var b backend.Backend
	if !opts.ForceLocal {
		var backendDiags tfdiags.Diagnostics
		b, backendDiags = m.backendFromConfig(opts)
		diags = diags.Append(backendDiags)

		if opts.Init && b != nil && !diags.HasErrors() {
			// Its possible that the currently selected workspace doesn't exist, so
			// we call selectWorkspace to ensure an existing workspace is selected.
			if err := m.selectWorkspace(b); err != nil {
				diags = diags.Append(err)
			}
		}

		if diags.HasErrors() {
			return nil, diags
		}

		log.Printf("[TRACE] Meta.Backend: instantiated backend of type %T", b)
	}

	// Set up the CLI opts we pass into backends that support it.
	cliOpts, err := m.backendCLIOpts()
	if err != nil {
		diags = diags.Append(err)
		return nil, diags
	}
	cliOpts.Validation = true

	// If the backend supports CLI initialization, do it.
	if cli, ok := b.(backend.CLI); ok {
		if err := cli.CLIInit(cliOpts); err != nil {
			diags = diags.Append(fmt.Errorf(
				"Error initializing backend %T: %s\n\n"+
					"This is a bug; please report it to the backend developer",
				b, err,
			))
			return nil, diags
		}
	}

	// If the result of loading the backend is an enhanced backend,
	// then return that as-is. This works even if b == nil (it will be !ok).
	if enhanced, ok := b.(backend.Enhanced); ok {
		log.Printf("[TRACE] Meta.Backend: backend %T supports operations", b)
		return enhanced, nil
	}

	// We either have a non-enhanced backend or no backend configured at
	// all. In either case, we use local as our enhanced backend and the
	// non-enhanced (if any) as the state backend.

	if !opts.ForceLocal {
		log.Printf("[TRACE] Meta.Backend: backend %T does not support operations, so wrapping it in a local backend", b)
	}

	// Build the local backend
	local := backendLocal.NewWithBackend(b)
	if err := local.CLIInit(cliOpts); err != nil {
		// Local backend isn't allowed to fail. It would be a bug.
		panic(err)
	}

	// If we got here from backendFromConfig returning nil then m.backendState
	// won't be set, since that codepath considers that to be no backend at all,
	// but our caller considers that to be the local backend with no config
	// and so we'll synthesize a backend state so other code doesn't need to
	// care about this special case.
	//
	// FIXME: We should refactor this so that we more directly and explicitly
	// treat the local backend as the default, including in the UI shown to
	// the user, since the local backend should only be used when learning or
	// in exceptional cases and so it's better to help the user learn that
	// by introducing it as a concept.
	if m.backendState == nil {
		// NOTE: This synthetic object is intentionally _not_ retained in the
		// on-disk record of the backend configuration, which was already dealt
		// with inside backendFromConfig, because we still need that codepath
		// to be able to recognize the lack of a config as distinct from
		// explicitly setting local until we do some more refactoring here.
		m.backendState = &legacy.BackendState{
			Type:      "local",
			ConfigRaw: json.RawMessage("{}"),
		}
	}

	return local, nil
}

// selectWorkspace gets a list of existing workspaces and then checks
// if the currently selected workspace is valid. If not, it will ask
// the user to select a workspace from the list.
func (m *Meta) selectWorkspace(b backend.Backend) error {
	workspaces, err := b.Workspaces()
	if err == backend.ErrWorkspacesNotSupported {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Failed to get existing workspaces: %s", err)
	}
	if len(workspaces) == 0 {
		return fmt.Errorf(strings.TrimSpace(errBackendNoExistingWorkspaces))
	}

	// Get the currently selected workspace.
	workspace, err := m.Workspace()
	if err != nil {
		return err
	}

	// Check if any of the existing workspaces matches the selected
	// workspace and create a numbered list of existing workspaces.
	var list strings.Builder
	for i, w := range workspaces {
		if w == workspace {
			log.Printf("[TRACE] Meta.selectWorkspace: the currently selected workspace is present in the configured backend (%s)", workspace)
			return nil
		}
		fmt.Fprintf(&list, "%d. %s\n", i+1, w)
	}

	// If the backend only has a single workspace, select that as the current workspace
	if len(workspaces) == 1 {
		log.Printf("[TRACE] Meta.selectWorkspace: automatically selecting the single workspace provided by the backend (%s)", workspaces[0])
		return m.SetWorkspace(workspaces[0])
	}

	// Otherwise, ask the user to select a workspace from the list of existing workspaces.
	v, err := m.UIInput().Input(context.Background(), &terraform.InputOpts{
		Id: "select-workspace",
		Query: fmt.Sprintf(
			"\n[reset][bold][yellow]The currently selected workspace (%s) does not exist.[reset]",
			workspace),
		Description: fmt.Sprintf(
			strings.TrimSpace(inputBackendSelectWorkspace), list.String()),
	})
	if err != nil {
		return fmt.Errorf("Failed to select workspace: %s", err)
	}

	idx, err := strconv.Atoi(v)
	if err != nil || (idx < 1 || idx > len(workspaces)) {
		return fmt.Errorf("Failed to select workspace: input not a valid number")
	}

	workspace = workspaces[idx-1]
	log.Printf("[TRACE] Meta.selectWorkspace: setting the current workpace according to user selection (%s)", workspace)
	return m.SetWorkspace(workspace)
}

// BackendForPlan is similar to Backend, but uses backend settings that were
// stored in a plan.
//
// The current workspace name is also stored as part of the plan, and so this
// method will check that it matches the currently-selected workspace name
// and produce error diagnostics if not.
func (m *Meta) BackendForPlan(settings plans.Backend) (backend.Enhanced, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	f := backendInit.Backend(settings.Type)
	if f == nil {
		diags = diags.Append(fmt.Errorf(strings.TrimSpace(errBackendSavedUnknown), settings.Type))
		return nil, diags
	}
	b := f()
	log.Printf("[TRACE] Meta.BackendForPlan: instantiated backend of type %T", b)

	schema := b.ConfigSchema()
	configVal, err := settings.Config.Decode(schema.ImpliedType())
	if err != nil {
		diags = diags.Append(fmt.Errorf("saved backend configuration is invalid: %w", err))
		return nil, diags
	}

	newVal, validateDiags := b.PrepareConfig(configVal)
	diags = diags.Append(validateDiags)
	if validateDiags.HasErrors() {
		return nil, diags
	}

	configureDiags := b.Configure(newVal)
	diags = diags.Append(configureDiags)

	// If the backend supports CLI initialization, do it.
	if cli, ok := b.(backend.CLI); ok {
		cliOpts, err := m.backendCLIOpts()
		if err != nil {
			diags = diags.Append(err)
			return nil, diags
		}
		if err := cli.CLIInit(cliOpts); err != nil {
			diags = diags.Append(fmt.Errorf(
				"Error initializing backend %T: %s\n\n"+
					"This is a bug; please report it to the backend developer",
				b, err,
			))
			return nil, diags
		}
	}

	// If the result of loading the backend is an enhanced backend,
	// then return that as-is. This works even if b == nil (it will be !ok).
	if enhanced, ok := b.(backend.Enhanced); ok {
		log.Printf("[TRACE] Meta.BackendForPlan: backend %T supports operations", b)
		return enhanced, nil
	}

	// Otherwise, we'll wrap our state-only remote backend in the local backend
	// to cause any operations to be run locally.
	log.Printf("[TRACE] Meta.Backend: backend %T does not support operations, so wrapping it in a local backend", b)
	cliOpts, err := m.backendCLIOpts()
	if err != nil {
		diags = diags.Append(err)
		return nil, diags
	}
	cliOpts.Validation = false // don't validate here in case config contains file(...) calls where the file doesn't exist
	local := backendLocal.NewWithBackend(b)
	if err := local.CLIInit(cliOpts); err != nil {
		// Local backend should never fail, so this is always a bug.
		panic(err)
	}

	return local, diags
}

// backendCLIOpts returns a backend.CLIOpts object that should be passed to
// a backend that supports local CLI operations.
func (m *Meta) backendCLIOpts() (*backend.CLIOpts, error) {
	contextOpts, err := m.contextOpts()
	if err != nil {
		return nil, err
	}
	return &backend.CLIOpts{
		CLI:                 m.Ui,
		CLIColor:            m.Colorize(),
		Streams:             m.Streams,
		StatePath:           m.statePath,
		StateOutPath:        m.stateOutPath,
		StateBackupPath:     m.backupPath,
		ContextOpts:         contextOpts,
		Input:               m.Input(),
		RunningInAutomation: m.RunningInAutomation,
	}, nil
}

// Operation initializes a new backend.Operation struct.
//
// This prepares the operation. After calling this, the caller is expected
// to modify fields of the operation such as Sequence to specify what will
// be called.
func (m *Meta) Operation(b backend.Backend) *backend.Operation {
	schema := b.ConfigSchema()
	workspace, err := m.Workspace()
	if err != nil {
		// An invalid workspace error would have been raised when creating the
		// backend, and the caller should have already exited. Seeing the error
		// here first is a bug, so panic.
		panic(fmt.Sprintf("invalid workspace: %s", err))
	}
	planOutBackend, err := m.backendState.ForPlan(schema, workspace)
	if err != nil {
		// Always indicates an implementation error in practice, because
		// errors here indicate invalid encoding of the backend configuration
		// in memory, and we should always have validated that by the time
		// we get here.
		panic(fmt.Sprintf("failed to encode backend configuration for plan: %s", err))
	}

	stateLocker := clistate.NewNoopLocker()
	if m.stateLock {
		view := views.NewStateLocker(arguments.ViewHuman, m.View)
		stateLocker = clistate.NewLocker(m.stateLockTimeout, view)
	}

	depLocks, diags := m.lockedDependencies()
	if diags.HasErrors() {
		// We can't actually report errors from here, but m.lockedDependencies
		// should always have been called earlier to prepare the "ContextOpts"
		// for the backend anyway, so we should never actually get here in
		// a real situation. If we do get here then the backend will inevitably
		// fail downstream somwhere if it tries to use the empty depLocks.
		log.Printf("[WARN] Failed to load dependency locks while preparing backend operation (ignored): %s", diags.Err().Error())
	}

	return &backend.Operation{
		PlanOutBackend:  planOutBackend,
		Targets:         m.targets,
		UIIn:            m.UIInput(),
		UIOut:           m.Ui,
		Workspace:       workspace,
		StateLocker:     stateLocker,
		DependencyLocks: depLocks,
	}
}

// backendConfig returns the local configuration for the backend
func (m *Meta) backendConfig(opts *BackendOpts) (*configs.Backend, int, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	if opts.Config == nil {
		// check if the config was missing, or just not required
		conf, moreDiags := m.loadBackendConfig(".")
		diags = diags.Append(moreDiags)
		if moreDiags.HasErrors() {
			return nil, 0, diags
		}

		if conf == nil {
			log.Println("[TRACE] Meta.Backend: no config given or present on disk, so returning nil config")
			return nil, 0, nil
		}

		log.Printf("[TRACE] Meta.Backend: BackendOpts.Config not set, so using settings loaded from %s", conf.DeclRange)
		opts.Config = conf
	}

	c := opts.Config

	if c == nil {
		log.Println("[TRACE] Meta.Backend: no explicit backend config, so returning nil config")
		return nil, 0, nil
	}

	bf := backendInit.Backend(c.Type)
	if bf == nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid backend type",
			Detail:   fmt.Sprintf("There is no backend type named %q.", c.Type),
			Subject:  &c.TypeRange,
		})
		return nil, 0, diags
	}
	b := bf()

	configSchema := b.ConfigSchema()
	configBody := c.Config
	configHash := c.Hash(configSchema)

	// If we have an override configuration body then we must apply it now.
	if opts.ConfigOverride != nil {
		log.Println("[TRACE] Meta.Backend: merging -backend-config=... CLI overrides into backend configuration")
		configBody = configs.MergeBodies(configBody, opts.ConfigOverride)
	}

	log.Printf("[TRACE] Meta.Backend: built configuration for %q backend with hash value %d", c.Type, configHash)

	// We'll shallow-copy configs.Backend here so that we can replace the
	// body without affecting others that hold this reference.
	configCopy := *c
	configCopy.Config = configBody
	return &configCopy, configHash, diags
}

// backendFromConfig returns the initialized (not configured) backend
// directly from the config/state..
//
// This function handles various edge cases around backend config loading. For
// example: new config changes, backend type changes, etc.
//
// As of the 0.12 release it can no longer migrate from legacy remote state
// to backends, and will instead instruct users to use 0.11 or earlier as
// a stepping-stone to do that migration.
//
// This function may query the user for input unless input is disabled, in
// which case this function will error.
func (m *Meta) backendFromConfig(opts *BackendOpts) (backend.Backend, tfdiags.Diagnostics) {
	// Get the local backend configuration.
	c, cHash, diags := m.backendConfig(opts)
	if diags.HasErrors() {
		return nil, diags
	}

	// ------------------------------------------------------------------------
	// For historical reasons, current backend configuration for a working
	// directory is kept in a *state-like* file, using the legacy state
	// structures in the Terraform package. It is not actually a Terraform
	// state, and so only the "backend" portion of it is actually used.
	//
	// The remainder of this code often confusingly refers to this as a "state",
	// so it's unfortunately important to remember that this is not actually
	// what we _usually_ think of as "state", and is instead a local working
	// directory "backend configuration state" that is never persisted anywhere.
	//
	// Since the "real" state has since moved on to be represented by
	// states.State, we can recognize the special meaning of state that applies
	// to this function and its callees by their continued use of the
	// otherwise-obsolete terraform.State.
	// ------------------------------------------------------------------------

	// Get the path to where we store a local cache of backend configuration
	// if we're using a remote backend. This may not yet exist which means
	// we haven't used a non-local backend before. That is okay.
	statePath := filepath.Join(m.DataDir(), DefaultStateFilename)
	sMgr := &clistate.LocalState{Path: statePath}
	if err := sMgr.RefreshState(); err != nil {
		diags = diags.Append(fmt.Errorf("Failed to load state: %s", err))
		return nil, diags
	}

	// Load the state, it must be non-nil for the tests below but can be empty
	s := sMgr.State()
	if s == nil {
		log.Printf("[TRACE] Meta.Backend: backend has not previously been initialized in this working directory")
		s = legacy.NewState()
	} else if s.Backend != nil {
		log.Printf("[TRACE] Meta.Backend: working directory was previously initialized for %q backend", s.Backend.Type)
	} else {
		log.Printf("[TRACE] Meta.Backend: working directory was previously initialized but has no backend (is using legacy remote state?)")
	}

	// if we want to force reconfiguration of the backend, we set the backend
	// state to nil on this copy. This will direct us through the correct
	// configuration path in the switch statement below.
	if m.reconfigure {
		s.Backend = nil
	}

	// Upon return, we want to set the state we're using in-memory so that
	// we can access it for commands.
	m.backendState = nil
	defer func() {
		if s := sMgr.State(); s != nil && !s.Backend.Empty() {
			m.backendState = s.Backend
		}
	}()

	if !s.Remote.Empty() {
		// Legacy remote state is no longer supported. User must first
		// migrate with Terraform 0.11 or earlier.
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Legacy remote state not supported",
			"This working directory is configured for legacy remote state, which is no longer supported from Terraform v0.12 onwards. To migrate this environment, first run \"terraform init\" under a Terraform 0.11 release, and then upgrade Terraform again.",
		))
		return nil, diags
	}

	// This switch statement covers all the different combinations of
	// configuring new backends, updating previously-configured backends, etc.
	switch {
	// No configuration set at all. Pure local state.
	case c == nil && s.Backend.Empty():
		log.Printf("[TRACE] Meta.Backend: using default local state only (no backend configuration, and no existing initialized backend)")
		return nil, nil

	// We're unsetting a backend (moving from backend => local)
	case c == nil && !s.Backend.Empty():
		log.Printf("[TRACE] Meta.Backend: previously-initialized %q backend is no longer present in config", s.Backend.Type)

		initReason := fmt.Sprintf("Unsetting the previously set backend %q", s.Backend.Type)
		if !opts.Init {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Backend initialization required, please run \"terraform init\"",
				fmt.Sprintf(strings.TrimSpace(errBackendInit), initReason),
			))
			return nil, diags
		}

		if !m.migrateState {
			diags = diags.Append(migrateOrReconfigDiag)
			return nil, diags
		}

		return m.backend_c_r_S(c, cHash, sMgr, true)

	// Configuring a backend for the first time.
	case c != nil && s.Backend.Empty():
		log.Printf("[TRACE] Meta.Backend: moving from default local state only to %q backend", c.Type)
		if !opts.Init {
			initReason := fmt.Sprintf("Initial configuration of the requested backend %q", c.Type)
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Backend initialization required, please run \"terraform init\"",
				fmt.Sprintf(strings.TrimSpace(errBackendInit), initReason),
			))
			return nil, diags
		}

		return m.backend_C_r_s(c, cHash, sMgr)

	// Potentially changing a backend configuration
	case c != nil && !s.Backend.Empty():
		// We are not going to migrate if were not initializing and the hashes
		// match indicating that the stored config is valid. If we are
		// initializing, then we also assume the the backend config is OK if
		// the hashes match, as long as we're not providing any new overrides.
		if (uint64(cHash) == s.Backend.Hash) && (!opts.Init || opts.ConfigOverride == nil) {
			log.Printf("[TRACE] Meta.Backend: using already-initialized, unchanged %q backend configuration", c.Type)
			return m.backend_C_r_S_unchanged(c, cHash, sMgr)
		}

		// If our configuration is the same, then we're just initializing
		// a previously configured remote backend.
		if !m.backendConfigNeedsMigration(c, s.Backend) {
			log.Printf("[TRACE] Meta.Backend: using already-initialized %q backend configuration", c.Type)
			return m.backend_C_r_S_unchanged(c, cHash, sMgr)
		}
		log.Printf("[TRACE] Meta.Backend: backend configuration has changed (from type %q to type %q)", s.Backend.Type, c.Type)

		initReason := fmt.Sprintf("Backend configuration changed for %q", c.Type)
		if s.Backend.Type != c.Type {
			initReason = fmt.Sprintf("Backend configuration changed from %q to %q", s.Backend.Type, c.Type)
		}

		if !opts.Init {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Backend initialization required, please run \"terraform init\"",
				fmt.Sprintf(strings.TrimSpace(errBackendInit), initReason),
			))
			return nil, diags
		}

		if !m.migrateState {
			diags = diags.Append(migrateOrReconfigDiag)
			return nil, diags
		}

		log.Printf("[WARN] backend config has changed since last init")
		return m.backend_C_r_S_changed(c, cHash, sMgr, true)

	default:
		diags = diags.Append(fmt.Errorf(
			"Unhandled backend configuration state. This is a bug. Please\n"+
				"report this error with the following information.\n\n"+
				"Config Nil: %v\n"+
				"Saved Backend Empty: %v\n",
			c == nil, s.Backend.Empty(),
		))
		return nil, diags
	}
}

// backendFromState returns the initialized (not configured) backend directly
// from the state. This should be used only when a user runs `terraform init
// -backend=false`. This function returns a local backend if there is no state
// or no backend configured.
func (m *Meta) backendFromState() (backend.Backend, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	// Get the path to where we store a local cache of backend configuration
	// if we're using a remote backend. This may not yet exist which means
	// we haven't used a non-local backend before. That is okay.
	statePath := filepath.Join(m.DataDir(), DefaultStateFilename)
	sMgr := &clistate.LocalState{Path: statePath}
	if err := sMgr.RefreshState(); err != nil {
		diags = diags.Append(fmt.Errorf("Failed to load state: %s", err))
		return nil, diags
	}
	s := sMgr.State()
	if s == nil {
		// no state, so return a local backend
		log.Printf("[TRACE] Meta.Backend: backend has not previously been initialized in this working directory")
		return backendLocal.New(), diags
	}
	if s.Backend == nil {
		// s.Backend is nil, so return a local backend
		log.Printf("[TRACE] Meta.Backend: working directory was previously initialized but has no backend (is using legacy remote state?)")
		return backendLocal.New(), diags
	}
	log.Printf("[TRACE] Meta.Backend: working directory was previously initialized for %q backend", s.Backend.Type)

	//backend init function
	if s.Backend.Type == "" {
		return backendLocal.New(), diags
	}
	f := backendInit.Backend(s.Backend.Type)
	if f == nil {
		diags = diags.Append(fmt.Errorf(strings.TrimSpace(errBackendSavedUnknown), s.Backend.Type))
		return nil, diags
	}
	b := f()

	// The configuration saved in the working directory state file is used
	// in this case, since it will contain any additional values that
	// were provided via -backend-config arguments on terraform init.
	schema := b.ConfigSchema()
	configVal, err := s.Backend.Config(schema)
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to decode current backend config",
			fmt.Sprintf("The backend configuration created by the most recent run of \"terraform init\" could not be decoded: %s. The configuration may have been initialized by an earlier version that used an incompatible configuration structure. Run \"terraform init -reconfigure\" to force re-initialization of the backend.", err),
		))
		return nil, diags
	}

	// Validate the config and then configure the backend
	newVal, validDiags := b.PrepareConfig(configVal)
	diags = diags.Append(validDiags)
	if validDiags.HasErrors() {
		return nil, diags
	}

	configDiags := b.Configure(newVal)
	diags = diags.Append(configDiags)
	if configDiags.HasErrors() {
		return nil, diags
	}

	return b, diags
}

//-------------------------------------------------------------------
// Backend Config Scenarios
//
// The functions below cover handling all the various scenarios that
// can exist when loading a backend. They are named in the format of
// "backend_C_R_S" where C, R, S may be upper or lowercase. Lowercase
// means it is false, uppercase means it is true. The full set of eight
// possible cases is handled.
//
// The fields are:
//
//   * C - Backend configuration is set and changed in TF files
//   * R - Legacy remote state is set
//   * S - Backend configuration is set in the state
//
//-------------------------------------------------------------------

// Unconfiguring a backend (moving from backend => local).
func (m *Meta) backend_c_r_S(c *configs.Backend, cHash int, sMgr *clistate.LocalState, output bool) (backend.Backend, tfdiags.Diagnostics) {
	s := sMgr.State()

	// Get the backend type for output
	backendType := s.Backend.Type

	m.Ui.Output(fmt.Sprintf(strings.TrimSpace(outputBackendMigrateLocal), s.Backend.Type))

	// Grab a purely local backend to get the local state if it exists
	localB, diags := m.Backend(&BackendOpts{ForceLocal: true})
	if diags.HasErrors() {
		return nil, diags
	}

	// Initialize the configured backend
	b, moreDiags := m.backend_C_r_S_unchanged(c, cHash, sMgr)
	diags = diags.Append(moreDiags)
	if moreDiags.HasErrors() {
		return nil, diags
	}

	// Perform the migration
	err := m.backendMigrateState(&backendMigrateOpts{
		OneType: s.Backend.Type,
		TwoType: "local",
		One:     b,
		Two:     localB,
	})
	if err != nil {
		diags = diags.Append(err)
		return nil, diags
	}

	// Remove the stored metadata
	s.Backend = nil
	if err := sMgr.WriteState(s); err != nil {
		diags = diags.Append(fmt.Errorf(strings.TrimSpace(errBackendClearSaved), err))
		return nil, diags
	}
	if err := sMgr.PersistState(); err != nil {
		diags = diags.Append(fmt.Errorf(strings.TrimSpace(errBackendClearSaved), err))
		return nil, diags
	}

	if output {
		m.Ui.Output(m.Colorize().Color(fmt.Sprintf(
			"[reset][green]\n\n"+
				strings.TrimSpace(successBackendUnset), backendType)))
	}

	// Return no backend
	return nil, diags
}

// Configuring a backend for the first time.
func (m *Meta) backend_C_r_s(c *configs.Backend, cHash int, sMgr *clistate.LocalState) (backend.Backend, tfdiags.Diagnostics) {
	// Get the backend
	b, configVal, diags := m.backendInitFromConfig(c)
	if diags.HasErrors() {
		return nil, diags
	}

	// Grab a purely local backend to get the local state if it exists
	localB, localBDiags := m.Backend(&BackendOpts{ForceLocal: true})
	if localBDiags.HasErrors() {
		diags = diags.Append(localBDiags)
		return nil, diags
	}

	workspaces, err := localB.Workspaces()
	if err != nil {
		diags = diags.Append(fmt.Errorf(errBackendLocalRead, err))
		return nil, diags
	}

	var localStates []statemgr.Full
	for _, workspace := range workspaces {
		localState, err := localB.StateMgr(workspace)
		if err != nil {
			diags = diags.Append(fmt.Errorf(errBackendLocalRead, err))
			return nil, diags
		}
		if err := localState.RefreshState(); err != nil {
			diags = diags.Append(fmt.Errorf(errBackendLocalRead, err))
			return nil, diags
		}

		// We only care about non-empty states.
		if localS := localState.State(); !localS.Empty() {
			log.Printf("[TRACE] Meta.Backend: will need to migrate workspace states because of existing %q workspace", workspace)
			localStates = append(localStates, localState)
		} else {
			log.Printf("[TRACE] Meta.Backend: ignoring local %q workspace because its state is empty", workspace)
		}
	}

	if len(localStates) > 0 {
		// Perform the migration
		err = m.backendMigrateState(&backendMigrateOpts{
			OneType: "local",
			TwoType: c.Type,
			One:     localB,
			Two:     b,
		})
		if err != nil {
			diags = diags.Append(err)
			return nil, diags
		}

		// we usually remove the local state after migration to prevent
		// confusion, but adding a default local backend block to the config
		// can get us here too. Don't delete our state if the old and new paths
		// are the same.
		erase := true
		if newLocalB, ok := b.(*backendLocal.Local); ok {
			if localB, ok := localB.(*backendLocal.Local); ok {
				if newLocalB.PathsConflictWith(localB) {
					erase = false
					log.Printf("[TRACE] Meta.Backend: both old and new backends share the same local state paths, so not erasing old state")
				}
			}
		}

		if erase {
			log.Printf("[TRACE] Meta.Backend: removing old state snapshots from old backend")
			for _, localState := range localStates {
				// We always delete the local state, unless that was our new state too.
				if err := localState.WriteState(nil); err != nil {
					diags = diags.Append(fmt.Errorf(errBackendMigrateLocalDelete, err))
					return nil, diags
				}
				if err := localState.PersistState(); err != nil {
					diags = diags.Append(fmt.Errorf(errBackendMigrateLocalDelete, err))
					return nil, diags
				}
			}
		}
	}

	if m.stateLock {
		view := views.NewStateLocker(arguments.ViewHuman, m.View)
		stateLocker := clistate.NewLocker(m.stateLockTimeout, view)
		if err := stateLocker.Lock(sMgr, "backend from plan"); err != nil {
			diags = diags.Append(fmt.Errorf("Error locking state: %s", err))
			return nil, diags
		}
		defer stateLocker.Unlock()
	}

	configJSON, err := ctyjson.Marshal(configVal, b.ConfigSchema().ImpliedType())
	if err != nil {
		diags = diags.Append(fmt.Errorf("Can't serialize backend configuration as JSON: %s", err))
		return nil, diags
	}

	// Store the metadata in our saved state location
	s := sMgr.State()
	if s == nil {
		s = legacy.NewState()
	}
	s.Backend = &legacy.BackendState{
		Type:      c.Type,
		ConfigRaw: json.RawMessage(configJSON),
		Hash:      uint64(cHash),
	}

	if err := sMgr.WriteState(s); err != nil {
		diags = diags.Append(fmt.Errorf(errBackendWriteSaved, err))
		return nil, diags
	}
	if err := sMgr.PersistState(); err != nil {
		diags = diags.Append(fmt.Errorf(errBackendWriteSaved, err))
		return nil, diags
	}

	// By now the backend is successfully configured.
	m.Ui.Output(m.Colorize().Color(fmt.Sprintf(
		"[reset][green]\n"+strings.TrimSpace(successBackendSet), s.Backend.Type)))

	return b, diags
}

// Changing a previously saved backend.
func (m *Meta) backend_C_r_S_changed(c *configs.Backend, cHash int, sMgr *clistate.LocalState, output bool) (backend.Backend, tfdiags.Diagnostics) {
	if output {
		// Notify the user
		m.Ui.Output(m.Colorize().Color(fmt.Sprintf(
			"[reset]%s\n\n",
			strings.TrimSpace(outputBackendReconfigure))))
	}

	// Get the old state
	s := sMgr.State()

	// Get the backend
	b, configVal, diags := m.backendInitFromConfig(c)
	if diags.HasErrors() {
		return nil, diags
	}

	// no need to confuse the user if the backend types are the same
	if s.Backend.Type != c.Type {
		m.Ui.Output(strings.TrimSpace(fmt.Sprintf(outputBackendMigrateChange, s.Backend.Type, c.Type)))
	}

	// Grab the existing backend
	oldB, oldBDiags := m.backend_C_r_S_unchanged(c, cHash, sMgr)
	diags = diags.Append(oldBDiags)
	if oldBDiags.HasErrors() {
		return nil, diags
	}

	// Perform the migration
	err := m.backendMigrateState(&backendMigrateOpts{
		OneType: s.Backend.Type,
		TwoType: c.Type,
		One:     oldB,
		Two:     b,
	})
	if err != nil {
		diags = diags.Append(err)
		return nil, diags
	}

	if m.stateLock {
		view := views.NewStateLocker(arguments.ViewHuman, m.View)
		stateLocker := clistate.NewLocker(m.stateLockTimeout, view)
		if err := stateLocker.Lock(sMgr, "backend from plan"); err != nil {
			diags = diags.Append(fmt.Errorf("Error locking state: %s", err))
			return nil, diags
		}
		defer stateLocker.Unlock()
	}

	configJSON, err := ctyjson.Marshal(configVal, b.ConfigSchema().ImpliedType())
	if err != nil {
		diags = diags.Append(fmt.Errorf("Can't serialize backend configuration as JSON: %s", err))
		return nil, diags
	}

	// Update the backend state
	s = sMgr.State()
	if s == nil {
		s = legacy.NewState()
	}
	s.Backend = &legacy.BackendState{
		Type:      c.Type,
		ConfigRaw: json.RawMessage(configJSON),
		Hash:      uint64(cHash),
	}

	if err := sMgr.WriteState(s); err != nil {
		diags = diags.Append(fmt.Errorf(errBackendWriteSaved, err))
		return nil, diags
	}
	if err := sMgr.PersistState(); err != nil {
		diags = diags.Append(fmt.Errorf(errBackendWriteSaved, err))
		return nil, diags
	}

	if output {
		m.Ui.Output(m.Colorize().Color(fmt.Sprintf(
			"[reset][green]\n"+strings.TrimSpace(successBackendSet), s.Backend.Type)))
	}

	return b, diags
}

// Initiailizing an unchanged saved backend
func (m *Meta) backend_C_r_S_unchanged(c *configs.Backend, cHash int, sMgr *clistate.LocalState) (backend.Backend, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	s := sMgr.State()

	// it's possible for a backend to be unchanged, and the config itself to
	// have changed by moving a parameter from the config to `-backend-config`
	// In this case we only need to update the Hash.
	if c != nil && s.Backend.Hash != uint64(cHash) {
		s.Backend.Hash = uint64(cHash)
		if err := sMgr.WriteState(s); err != nil {
			diags = diags.Append(err)
			return nil, diags
		}
	}

	// Get the backend
	f := backendInit.Backend(s.Backend.Type)
	if f == nil {
		diags = diags.Append(fmt.Errorf(strings.TrimSpace(errBackendSavedUnknown), s.Backend.Type))
		return nil, diags
	}
	b := f()

	// The configuration saved in the working directory state file is used
	// in this case, since it will contain any additional values that
	// were provided via -backend-config arguments on terraform init.
	schema := b.ConfigSchema()
	configVal, err := s.Backend.Config(schema)
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to decode current backend config",
			fmt.Sprintf("The backend configuration created by the most recent run of \"terraform init\" could not be decoded: %s. The configuration may have been initialized by an earlier version that used an incompatible configuration structure. Run \"terraform init -reconfigure\" to force re-initialization of the backend.", err),
		))
		return nil, diags
	}

	// Validate the config and then configure the backend
	newVal, validDiags := b.PrepareConfig(configVal)
	diags = diags.Append(validDiags)
	if validDiags.HasErrors() {
		return nil, diags
	}

	configDiags := b.Configure(newVal)
	diags = diags.Append(configDiags)
	if configDiags.HasErrors() {
		return nil, diags
	}

	return b, diags
}

//-------------------------------------------------------------------
// Reusable helper functions for backend management
//-------------------------------------------------------------------

// backendConfigNeedsMigration returns true if migration might be required to
// move from the configured backend to the given cached backend config.
//
// This must be called with the synthetic *configs.Backend that results from
// merging in any command-line options for correct behavior.
//
// If either the given configuration or cached configuration are invalid then
// this function will conservatively assume that migration is required,
// expecting that the migration code will subsequently deal with the same
// errors.
func (m *Meta) backendConfigNeedsMigration(c *configs.Backend, s *legacy.BackendState) bool {
	if s == nil || s.Empty() {
		log.Print("[TRACE] backendConfigNeedsMigration: no cached config, so migration is required")
		return true
	}
	if c.Type != s.Type {
		log.Printf("[TRACE] backendConfigNeedsMigration: type changed from %q to %q, so migration is required", s.Type, c.Type)
		return true
	}

	// We need the backend's schema to do our comparison here.
	f := backendInit.Backend(c.Type)
	if f == nil {
		log.Printf("[TRACE] backendConfigNeedsMigration: no backend of type %q, which migration codepath must handle", c.Type)
		return true // let the migration codepath deal with the missing backend
	}
	b := f()

	schema := b.ConfigSchema()
	decSpec := schema.NoneRequired().DecoderSpec()
	givenVal, diags := hcldec.Decode(c.Config, decSpec, nil)
	if diags.HasErrors() {
		log.Printf("[TRACE] backendConfigNeedsMigration: failed to decode given config; migration codepath must handle problem: %s", diags.Error())
		return true // let the migration codepath deal with these errors
	}

	cachedVal, err := s.Config(schema)
	if err != nil {
		log.Printf("[TRACE] backendConfigNeedsMigration: failed to decode cached config; migration codepath must handle problem: %s", err)
		return true // let the migration codepath deal with the error
	}

	// If we get all the way down here then it's the exact equality of the
	// two decoded values that decides our outcome. It's safe to use RawEquals
	// here (rather than Equals) because we know that unknown values can
	// never appear in backend configurations.
	if cachedVal.RawEquals(givenVal) {
		log.Print("[TRACE] backendConfigNeedsMigration: given configuration matches cached configuration, so no migration is required")
		return false
	}
	log.Print("[TRACE] backendConfigNeedsMigration: configuration values have changed, so migration is required")
	return true
}

func (m *Meta) backendInitFromConfig(c *configs.Backend) (backend.Backend, cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// Get the backend
	f := backendInit.Backend(c.Type)
	if f == nil {
		diags = diags.Append(fmt.Errorf(strings.TrimSpace(errBackendNewUnknown), c.Type))
		return nil, cty.NilVal, diags
	}
	b := f()

	schema := b.ConfigSchema()
	decSpec := schema.NoneRequired().DecoderSpec()
	configVal, hclDiags := hcldec.Decode(c.Config, decSpec, nil)
	diags = diags.Append(hclDiags)
	if hclDiags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	// TODO: test
	if m.Input() {
		var err error
		configVal, err = m.inputForSchema(configVal, schema)
		if err != nil {
			diags = diags.Append(fmt.Errorf("Error asking for input to configure backend %q: %s", c.Type, err))
		}

		// We get an unknown here if the if the user aborted input, but we can't
		// turn that into a config value, so set it to null and let the provider
		// handle it in PrepareConfig.
		if !configVal.IsKnown() {
			configVal = cty.NullVal(configVal.Type())
		}
	}

	newVal, validateDiags := b.PrepareConfig(configVal)
	diags = diags.Append(validateDiags.InConfigBody(c.Config, ""))
	if validateDiags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	configureDiags := b.Configure(newVal)
	diags = diags.Append(configureDiags.InConfigBody(c.Config, ""))

	return b, configVal, diags
}

// Helper method to ignore remote backend version conflicts. Only call this
// for commands which cannot accidentally upgrade remote state files.
func (m *Meta) ignoreRemoteBackendVersionConflict(b backend.Backend) {
	if rb, ok := b.(*remoteBackend.Remote); ok {
		rb.IgnoreVersionConflict()
	}
}

// Helper method to check the local Terraform version against the configured
// version in the remote workspace, returning diagnostics if they conflict.
func (m *Meta) remoteBackendVersionCheck(b backend.Backend, workspace string) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if rb, ok := b.(*remoteBackend.Remote); ok {
		// Allow user override based on command-line flag
		if m.ignoreRemoteVersion {
			rb.IgnoreVersionConflict()
		}
		// If the override is set, this check will return a warning instead of
		// an error
		versionDiags := rb.VerifyWorkspaceTerraformVersion(workspace)
		diags = diags.Append(versionDiags)
		// If there are no errors resulting from this check, we do not need to
		// check again
		if !diags.HasErrors() {
			rb.IgnoreVersionConflict()
		}
	}

	return diags
}

//-------------------------------------------------------------------
// Output constants and initialization code
//-------------------------------------------------------------------

const errBackendLocalRead = `
Error reading local state: %s

Terraform is trying to read your local state to determine if there is
state to migrate to your newly configured backend. Terraform can't continue
without this check because that would risk losing state. Please resolve the
error above and try again.
`

const errBackendMigrateLocalDelete = `
Error deleting local state after migration: %s

Your local state is deleted after successfully migrating it to the newly
configured backend. As part of the deletion process, a backup is made at
the standard backup path unless explicitly asked not to. To cleanly operate
with a backend, we must delete the local state file. Please resolve the
issue above and retry the command.
`

const errBackendNewUnknown = `
The backend %q could not be found.

This is the backend specified in your Terraform configuration file.
This error could be a simple typo in your configuration, but it can also
be caused by using a Terraform version that doesn't support the specified
backend type. Please check your configuration and your Terraform version.

If you'd like to run Terraform and store state locally, you can fix this
error by removing the backend configuration from your configuration.
`

const errBackendNoExistingWorkspaces = `
No existing workspaces.

Use the "terraform workspace" command to create and select a new workspace.
If the backend already contains existing workspaces, you may need to update
the backend configuration.
`

const errBackendSavedUnknown = `
The backend %q could not be found.

This is the backend that this Terraform environment is configured to use
both in your configuration and saved locally as your last-used backend.
If it isn't found, it could mean an alternate version of Terraform was
used with this configuration. Please use the proper version of Terraform that
contains support for this backend.

If you'd like to force remove this backend, you must update your configuration
to not use the backend and run "terraform init" (or any other command) again.
`

const errBackendClearSaved = `
Error clearing the backend configuration: %s

Terraform removes the saved backend configuration when you're removing a
configured backend. This must be done so future Terraform runs know to not
use the backend configuration. Please look at the error above, resolve it,
and try again.
`

const errBackendInit = `
Reason: %s

The "backend" is the interface that Terraform uses to store state,
perform operations, etc. If this message is showing up, it means that the
Terraform configuration you're using is using a custom configuration for
the Terraform backend.

Changes to backend configurations require reinitialization. This allows
Terraform to set up the new configuration, copy existing state, etc. Please run
"terraform init" with either the "-reconfigure" or "-migrate-state" flags to
use the current configuration.

If the change reason above is incorrect, please verify your configuration
hasn't changed and try again. At this point, no changes to your existing
configuration or state have been made.
`

const errBackendWriteSaved = `
Error saving the backend configuration: %s

Terraform saves the complete backend configuration in a local file for
configuring the backend on future operations. This cannot be disabled. Errors
are usually due to simple file permission errors. Please look at the error
above, resolve it, and try again.
`

const outputBackendMigrateChange = `
Terraform detected that the backend type changed from %q to %q.
`

const outputBackendMigrateLocal = `
Terraform has detected you're unconfiguring your previously set %q backend.
`

const outputBackendReconfigure = `
[reset][bold]Backend configuration changed![reset]

Terraform has detected that the configuration specified for the backend
has changed. Terraform will now check for existing state in the backends.
`

const successBackendUnset = `
Successfully unset the backend %q. Terraform will now operate locally.
`

const successBackendSet = `
Successfully configured the backend %q! Terraform will automatically
use this backend unless the backend configuration changes.
`

var migrateOrReconfigDiag = tfdiags.Sourceless(
	tfdiags.Error,
	"Backend configuration changed",
	"A change in the backend configuration has been detected, which may require migrating existing state.\n\n"+
		"If you wish to attempt automatic migration of the state, use \"terraform init -migrate-state\".\n"+
		`If you wish to store the current configuration with no changes to the state, use "terraform init -reconfigure".`)
