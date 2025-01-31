package keeper

import (
	"encoding/binary"
	"encoding/json"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/tendermint/tendermint/libs/log"
	tmos "github.com/tendermint/tendermint/libs/os"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/store/prefix"
	store "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/types/module"
	vm "github.com/cosmos/cosmos-sdk/x/upgrade/exported"
	"github.com/cosmos/cosmos-sdk/x/upgrade/types"
)

// UpgradeInfoFileName file to store upgrade information
const UpgradeInfoFileName string = "upgrade-info.json"

type Keeper struct {
	homePath           string                          // root directory of app config
	skipUpgradeHeights map[int64]bool                  // map of heights to skip for an upgrade
	storeKey           sdk.StoreKey                    // key to access x/upgrade store
	cdc                codec.BinaryCodec               // App-wide binary codec
	upgradeHandlers    map[string]types.UpgradeHandler // map of plan name to upgrade handler
	versionManager     vm.AppVersionManager            // implements setting the app version field on BaseApp
}

// NewKeeper constructs an upgrade Keeper which requires the following arguments:
// skipUpgradeHeights - map of heights to skip an upgrade
// storeKey - a store key with which to access upgrade's store
// cdc - the app-wide binary codec
// homePath - root directory of the application's config
// vs - the interface implemented by baseapp which allows setting baseapp's app version field
func NewKeeper(skipUpgradeHeights map[int64]bool, storeKey sdk.StoreKey, cdc codec.BinaryCodec, homePath string, vs vm.AppVersionManager) Keeper {
	return Keeper{
		homePath:           homePath,
		skipUpgradeHeights: skipUpgradeHeights,
		storeKey:           storeKey,
		cdc:                cdc,
		upgradeHandlers:    map[string]types.UpgradeHandler{},
		versionManager:     vs,
	}
}

// SetUpgradeHandler sets an UpgradeHandler for the upgrade specified by name. This handler will be called when the upgrade
// with this name is applied. In order for an upgrade with the given name to proceed, a handler for this upgrade
// must be set even if it is a no-op function.
func (k Keeper) SetUpgradeHandler(name string, upgradeHandler types.UpgradeHandler) {
	k.upgradeHandlers[name] = upgradeHandler
}

// SetModuleVersionMap saves a given version map to state
func (k Keeper) SetModuleVersionMap(ctx sdk.Context, vm module.VersionMap) {
	if len(vm) > 0 {
		store := ctx.KVStore(k.storeKey)
		versionStore := prefix.NewStore(store, []byte{types.VersionMapByte})
		// Even though the underlying store (cachekv) store is sorted, we still
		// prefer a deterministic iteration order of the map, to avoid undesired
		// surprises if we ever change stores.
		sortedModNames := make([]string, 0, len(vm))

		for key := range vm {
			sortedModNames = append(sortedModNames, key)
		}
		sort.Strings(sortedModNames)

		for _, modName := range sortedModNames {
			ver := vm[modName]
			nameBytes := []byte(modName)
			verBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(verBytes, ver)
			versionStore.Set(nameBytes, verBytes)
		}
	}
}

// SetAppVersion sets app version to version
func (k Keeper) SetAppVersion(ctx sdk.Context, version uint64) error {
	return k.versionManager.SetAppVersion(version)
}

// GetModuleVersionMap returns a map of key module name and value module consensus version
// as defined in ADR-041.
func (k Keeper) GetModuleVersionMap(ctx sdk.Context) module.VersionMap {
	store := ctx.KVStore(k.storeKey)
	it := sdk.KVStorePrefixIterator(store, []byte{types.VersionMapByte})

	vm := make(module.VersionMap)
	defer it.Close()
	for ; it.Valid(); it.Next() {
		moduleBytes := it.Key()
		// first byte is prefix key, so we remove it here
		name := string(moduleBytes[1:])
		moduleVersion := binary.BigEndian.Uint64(it.Value())
		vm[name] = moduleVersion
	}

	return vm
}

// GetModuleVersions gets a slice of module consensus versions
func (k Keeper) GetModuleVersions(ctx sdk.Context) []*types.ModuleVersion {
	store := ctx.KVStore(k.storeKey)
	it := sdk.KVStorePrefixIterator(store, []byte{types.VersionMapByte})
	defer it.Close()

	mv := make([]*types.ModuleVersion, 0)
	for ; it.Valid(); it.Next() {
		moduleBytes := it.Key()
		name := string(moduleBytes[1:])
		moduleVersion := binary.BigEndian.Uint64(it.Value())
		mv = append(mv, &types.ModuleVersion{
			Name:    name,
			Version: moduleVersion,
		})
	}
	return mv
}

// gets the version for a given module, and returns true if it exists, false otherwise
func (k Keeper) getModuleVersion(ctx sdk.Context, name string) (uint64, bool) {
	store := ctx.KVStore(k.storeKey)
	it := sdk.KVStorePrefixIterator(store, []byte{types.VersionMapByte})
	defer it.Close()

	for ; it.Valid(); it.Next() {
		moduleName := string(it.Key()[1:])
		if moduleName == name {
			version := binary.BigEndian.Uint64(it.Value())
			return version, true
		}
	}
	return 0, false
}

// ScheduleUpgrade schedules an upgrade based on the specified plan.
// If there is another Plan already scheduled, it will overwrite it
// (implicitly cancelling the current plan)
// ScheduleUpgrade will also write the upgraded IBC ClientState to the upgraded client
// path if it is specified in the plan.
func (k Keeper) ScheduleUpgrade(ctx sdk.Context, plan types.Plan) error {
	if err := plan.ValidateBasic(); err != nil {
		return err
	}

	// NOTE: allow for the possibility of chains to schedule upgrades in begin block of the same block
	// as a strategy for emergency hard fork recoveries
	if plan.Height < ctx.BlockHeight() {
		return sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "upgrade cannot be scheduled in the past")
	}

	if k.GetDoneHeight(ctx, plan.Name) != 0 {
		return sdkerrors.Wrapf(sdkerrors.ErrInvalidRequest, "upgrade with name %s has already been completed", plan.Name)
	}

	store := ctx.KVStore(k.storeKey)

	// clear any old IBC state stored by previous plan
	oldPlan, found := k.GetUpgradePlan(ctx)
	if found {
		k.ClearIBCState(ctx, oldPlan.Height)
	}

	bz := k.cdc.MustMarshal(&plan)
	store.Set(types.PlanKey(), bz)

	return nil
}

// SetUpgradedClient sets the expected upgraded client for the next version of this chain at the last height the current chain will commit.
func (k Keeper) SetUpgradedClient(ctx sdk.Context, planHeight int64, bz []byte) error {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.UpgradedClientKey(planHeight), bz)
	return nil
}

// GetUpgradedClient gets the expected upgraded client for the next version of this chain
func (k Keeper) GetUpgradedClient(ctx sdk.Context, height int64) ([]byte, bool) {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(types.UpgradedClientKey(height))
	if len(bz) == 0 {
		return nil, false
	}

	return bz, true
}

// SetUpgradedConsensusState set the expected upgraded consensus state for the next version of this chain
// using the last height committed on this chain.
func (k Keeper) SetUpgradedConsensusState(ctx sdk.Context, planHeight int64, bz []byte) error {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.UpgradedConsStateKey(planHeight), bz)
	return nil
}

// GetUpgradedConsensusState set the expected upgraded consensus state for the next version of this chain
func (k Keeper) GetUpgradedConsensusState(ctx sdk.Context, lastHeight int64) ([]byte, bool) {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(types.UpgradedConsStateKey(lastHeight))
	if len(bz) == 0 {
		return nil, false
	}

	return bz, true
}

// GetDoneHeight returns the height at which the given upgrade was executed
func (k Keeper) GetDoneHeight(ctx sdk.Context, name string) int64 {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), []byte{types.DoneByte})
	bz := store.Get([]byte(name))
	if len(bz) == 0 {
		return 0
	}

	return int64(binary.BigEndian.Uint64(bz))
}

// ClearIBCState clears any planned IBC state
func (k Keeper) ClearIBCState(ctx sdk.Context, lastHeight int64) {
	// delete IBC client and consensus state from store if this is IBC plan
	store := ctx.KVStore(k.storeKey)
	store.Delete(types.UpgradedClientKey(lastHeight))
	store.Delete(types.UpgradedConsStateKey(lastHeight))
}

// ClearUpgradePlan clears any schedule upgrade and associated IBC states.
func (k Keeper) ClearUpgradePlan(ctx sdk.Context) {
	// clear IBC states everytime upgrade plan is removed
	oldPlan, found := k.GetUpgradePlan(ctx)
	if found {
		k.ClearIBCState(ctx, oldPlan.Height)
	}

	store := ctx.KVStore(k.storeKey)
	store.Delete(types.PlanKey())
}

// Logger returns a module-specific logger.
func (k Keeper) Logger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", "x/"+types.ModuleName)
}

// GetUpgradePlan returns the currently scheduled Plan if any, setting havePlan to true if there is a scheduled
// upgrade or false if there is none
func (k Keeper) GetUpgradePlan(ctx sdk.Context) (plan types.Plan, havePlan bool) {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(types.PlanKey())
	if bz == nil {
		return plan, false
	}

	k.cdc.MustUnmarshal(bz, &plan)
	return plan, true
}

// setDone marks this upgrade name as being done so the name can't be reused accidentally
func (k Keeper) setDone(ctx sdk.Context, name string) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), []byte{types.DoneByte})
	bz := make([]byte, 8)
	binary.BigEndian.PutUint64(bz, uint64(ctx.BlockHeight()))
	store.Set([]byte(name), bz)
}

// HasHandler returns true iff there is a handler registered for this name
func (k Keeper) HasHandler(name string) bool {
	_, ok := k.upgradeHandlers[name]
	return ok
}

// ApplyUpgrade will execute the handler associated with the Plan and mark the plan as done.
func (k Keeper) ApplyUpgrade(ctx sdk.Context, plan types.Plan) {
	handler := k.upgradeHandlers[plan.Name]
	if handler == nil {
		panic("ApplyUpgrade should never be called without first checking HasHandler")
	}

	updatedVM, err := handler(ctx, plan, k.GetModuleVersionMap(ctx))
	if err != nil {
		panic(err)
	}

	k.SetModuleVersionMap(ctx, updatedVM)

	if k.versionManager != nil {
		// increment the app version and set it in state and baseapp
		appVersion, err := k.versionManager.GetAppVersion()
		if err != nil {
			panic(err)
		}
		if err := k.versionManager.SetAppVersion(appVersion + 1); err != nil {
			panic(err)
		}
	}

	// Must clear IBC state after upgrade is applied as it is stored separately from the upgrade plan.
	// This will prevent resubmission of upgrade msg after upgrade is already completed.
	k.ClearIBCState(ctx, plan.Height)
	k.ClearUpgradePlan(ctx)
	k.setDone(ctx, plan.Name)
}

// IsSkipHeight checks if the given height is part of skipUpgradeHeights
func (k Keeper) IsSkipHeight(height int64) bool {
	return k.skipUpgradeHeights[height]
}

// DumpUpgradeInfoToDisk writes upgrade information to UpgradeInfoFileName. The function
// doesn't save the `Plan.Info` data, hence it won't support auto download functionality
// by cosmvisor.
// NOTE: this function will be update in the next release.
func (k Keeper) DumpUpgradeInfoToDisk(height int64, name string) error {
	return k.DumpUpgradeInfoWithInfoToDisk(height, name, "")
}

// Deprecated: DumpUpgradeInfoWithInfoToDisk writes upgrade information to UpgradeInfoFileName.
// `info` should be provided and contain Plan.Info data in order to support
// auto download functionality by cosmovisor and other tools using upgarde-info.json
// (GetUpgradeInfoPath()) file.
func (k Keeper) DumpUpgradeInfoWithInfoToDisk(height int64, name string, info string) error {
	upgradeInfoFilePath, err := k.GetUpgradeInfoPath()
	if err != nil {
		return err
	}

	upgradeInfo := upgradeInfo{
		Name:   name,
		Height: height,
		Info:   info,
	}
	bz, err := json.Marshal(upgradeInfo)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(upgradeInfoFilePath, bz, 0600)
}

// GetUpgradeInfoPath returns the upgrade info file path
func (k Keeper) GetUpgradeInfoPath() (string, error) {
	upgradeInfoFileDir := path.Join(k.getHomeDir(), "data")
	err := tmos.EnsureDir(upgradeInfoFileDir, os.ModePerm)
	if err != nil {
		return "", err
	}

	return filepath.Join(upgradeInfoFileDir, UpgradeInfoFileName), nil
}

// getHomeDir returns the height at which the given upgrade was executed
func (k Keeper) getHomeDir() string {
	return k.homePath
}

// ReadUpgradeInfoFromDisk returns the name and height of the upgrade which is
// written to disk by the old binary when panicking. An error is returned if
// the upgrade path directory cannot be created or if the file exists and
// cannot be read or if the upgrade info fails to unmarshal.
func (k Keeper) ReadUpgradeInfoFromDisk() (store.UpgradeInfo, error) {
	var upgradeInfo store.UpgradeInfo

	upgradeInfoPath, err := k.GetUpgradeInfoPath()
	if err != nil {
		return upgradeInfo, err
	}

	data, err := ioutil.ReadFile(upgradeInfoPath)
	if err != nil {
		// if file does not exist, assume there are no upgrades
		if os.IsNotExist(err) {
			return upgradeInfo, nil
		}

		return upgradeInfo, err
	}

	if err := json.Unmarshal(data, &upgradeInfo); err != nil {
		return upgradeInfo, err
	}

	return upgradeInfo, nil
}

// upgradeInfo is stripped types.Plan structure used to dump upgrade plan data.
type upgradeInfo struct {
	// Name has types.Plan.Name value
	Name string `json:"name,omitempty"`
	// Height has types.Plan.Height value
	Height int64 `json:"height,omitempty"`
	// Height has types.Plan.Height value
	Info string `json:"info,omitempty"`
}
