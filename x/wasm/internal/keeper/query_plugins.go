package keeper

import (
	"encoding/json"
	wasmTypes "github.com/CosmWasm/go-cosmwasm/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/staking"
)

type QueryHandler struct {
	Ctx     sdk.Context
	Plugins QueryPlugins
}

var _ wasmTypes.Querier = QueryHandler{}

func (q QueryHandler) Query(request wasmTypes.QueryRequest) ([]byte, error) {
	if request.Bank != nil {
		return q.Plugins.Bank(q.Ctx, request.Bank)
	}
	if request.Custom != nil {
		return q.Plugins.Custom(q.Ctx, request.Custom)
	}
	if request.Staking != nil {
		return q.Plugins.Staking(q.Ctx, request.Staking)
	}
	if request.Wasm != nil {
		return q.Plugins.Wasm(q.Ctx, request.Wasm)
	}
	return nil, wasmTypes.Unknown{}
}

type CustomQuerier func(ctx sdk.Context, request json.RawMessage) ([]byte, error)

type QueryPlugins struct {
	Bank    func(ctx sdk.Context, request *wasmTypes.BankQuery) ([]byte, error)
	Custom  CustomQuerier
	Staking func(ctx sdk.Context, request *wasmTypes.StakingQuery) ([]byte, error)
	Wasm    func(ctx sdk.Context, request *wasmTypes.WasmQuery) ([]byte, error)
}

func DefaultQueryPlugins(bank bank.ViewKeeper, staking staking.Keeper, wasm Keeper) QueryPlugins {
	return QueryPlugins{
		Bank:    BankQuerier(bank),
		Custom:  NoCustomQuerier,
		Staking: StakingQuerier(staking),
		Wasm:    WasmQuerier(wasm),
	}
}

func (e QueryPlugins) Merge(o *QueryPlugins) QueryPlugins {
	// only update if this is non-nil and then only set values
	if o == nil {
		return e
	}
	if o.Bank != nil {
		e.Bank = o.Bank
	}
	if o.Custom != nil {
		e.Custom = o.Custom
	}
	if o.Staking != nil {
		e.Staking = o.Staking
	}
	if o.Wasm != nil {
		e.Wasm = o.Wasm
	}
	return e
}

func BankQuerier(bank bank.ViewKeeper) func(ctx sdk.Context, request *wasmTypes.BankQuery) ([]byte, error) {
	return func(ctx sdk.Context, request *wasmTypes.BankQuery) ([]byte, error) {
		if request.AllBalances != nil {
			addr, err := sdk.AccAddressFromBech32(request.AllBalances.Address)
			if err != nil {
				return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, request.AllBalances.Address)
			}
			coins := bank.GetCoins(ctx, addr)
			res := wasmTypes.AllBalancesResponse{
				Amount: convertSdkCoinsToWasmCoins(coins),
			}
			return json.Marshal(res)
		}
		if request.Balance != nil {
			addr, err := sdk.AccAddressFromBech32(request.Balance.Address)
			if err != nil {
				return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, request.Balance.Address)
			}
			coins := bank.GetCoins(ctx, addr)
			amount := coins.AmountOf(request.Balance.Denom)
			res := wasmTypes.BalanceResponse{
				Amount: wasmTypes.Coin{
					Denom:  request.Balance.Denom,
					Amount: amount.String(),
				},
			}
			return json.Marshal(res)
		}
		return nil, wasmTypes.UnsupportedRequest{"unknown BankQuery variant"}
	}
}

func NoCustomQuerier(ctx sdk.Context, request json.RawMessage) ([]byte, error) {
	return nil, wasmTypes.UnsupportedRequest{"custom"}
}

func StakingQuerier(keeper staking.Keeper) func(ctx sdk.Context, request *wasmTypes.StakingQuery) ([]byte, error) {
	return func(ctx sdk.Context, request *wasmTypes.StakingQuery) ([]byte, error) {
		if request.BondedDenom != nil {
			denom := keeper.BondDenom(ctx)
			res := wasmTypes.BondedDenomResponse{
				Denom: denom,
			}
			return json.Marshal(res)
		}
		if request.Validators != nil {
			validators := keeper.GetBondedValidatorsByPower(ctx)
			//validators := keeper.GetAllValidators(ctx)
			wasmVals := make([]wasmTypes.Validator, len(validators))
			for i, v := range validators {
				wasmVals[i] = wasmTypes.Validator{
					Address:       v.OperatorAddress.String(),
					Commission:    v.Commission.Rate.String(),
					MaxCommission: v.Commission.MaxRate.String(),
					MaxChangeRate: v.Commission.MaxChangeRate.String(),
				}
			}
			res := wasmTypes.ValidatorsResponse{
				Validators: wasmVals,
			}
			return json.Marshal(res)
		}
		if request.Delegations != nil {
			delegator, err := sdk.AccAddressFromBech32(request.Delegations.Delegator)
			if err != nil {
				return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, request.Delegations.Delegator)
			}
			var validator sdk.ValAddress
			validator, err = sdk.ValAddressFromBech32(request.Delegations.Validator)
			if err != nil {
				return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, request.Delegations.Validator)
			}

			delegations, err := calculateDelegations(ctx, keeper, delegator, validator)
			if err != nil {
				return nil, err
			}
			res := wasmTypes.DelegationsResponse{
				Delegations: delegations,
			}
			return json.Marshal(res)
		}
		return nil, wasmTypes.UnsupportedRequest{"unknown Staking variant"}
	}
}

// calculateDelegations does all queries needed to get validation info
// delegator must be set, if validator is not set, get all delegations by delegator
func calculateDelegations(ctx sdk.Context, keeper staking.Keeper, delegator sdk.AccAddress, validator sdk.ValAddress) (wasmTypes.Delegations, error) {
	// get delegations (either by validator, or over all validators)
	var sdkDels []staking.Delegation
	if len(validator) == 0 {
		sdkDels = keeper.GetAllDelegatorDelegations(ctx, delegator)
	} else {
		d, found := keeper.GetDelegation(ctx, delegator, validator)
		if found {
			sdkDels = []staking.Delegation{d}
		}
	}

	// convert them to the cosmwasm format, pulling in extra info as needed
	delegations := make([]wasmTypes.Delegation, len(sdkDels))
	for i, d := range sdkDels {
		// shares to amount logic comes from here:
		// https://github.com/cosmos/cosmos-sdk/blob/v0.38.3/x/staking/keeper/querier.go#L404
		val, found := keeper.GetValidator(ctx, d.ValidatorAddress)
		if !found {
			return nil, sdkerrors.Wrap(staking.ErrNoValidatorFound, "can't load validator for delegation")
		}
		bondDenom := keeper.BondDenom(ctx)
		amount := sdk.NewCoin(bondDenom, val.TokensFromShares(d.Shares).TruncateInt())

		// Accumulated Rewards???

		// can relegate? other query for redelegations?
		// keeper.GetRedelegation

		delegations[i] = wasmTypes.Delegation{
			Delegator: d.DelegatorAddress.String(),
			Validator: d.ValidatorAddress.String(),
			Amount:    convertSdkCoinToWasmCoin(amount),
			// TODO: AccumulatedRewards
			AccumulatedRewards: wasmTypes.NewCoin(0, bondDenom),
			// TODO: Determine redelegate
			CanRedelegate: false,
		}
	}
	return delegations, nil
}

func WasmQuerier(wasm Keeper) func(ctx sdk.Context, request *wasmTypes.WasmQuery) ([]byte, error) {
	return func(ctx sdk.Context, request *wasmTypes.WasmQuery) ([]byte, error) {
		if request.Smart != nil {
			addr, err := sdk.AccAddressFromBech32(request.Smart.ContractAddr)
			if err != nil {
				return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, request.Smart.ContractAddr)
			}
			return wasm.QuerySmart(ctx, addr, request.Smart.Msg)
		}
		if request.Raw != nil {
			addr, err := sdk.AccAddressFromBech32(request.Raw.ContractAddr)
			if err != nil {
				return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, request.Raw.ContractAddr)
			}
			models := wasm.QueryRaw(ctx, addr, request.Raw.Key)
			// TODO: do we want to change the return value?
			return json.Marshal(models)
		}
		return nil, wasmTypes.UnsupportedRequest{"unknown WasmQuery variant"}
	}
}

func convertSdkCoinsToWasmCoins(coins []sdk.Coin) wasmTypes.Coins {
	converted := make(wasmTypes.Coins, len(coins))
	for i, c := range coins {
		converted[i] = convertSdkCoinToWasmCoin(c)
	}
	return converted
}

func convertSdkCoinToWasmCoin(coin sdk.Coin) wasmTypes.Coin {
	return wasmTypes.Coin{
		Denom:  coin.Denom,
		Amount: coin.Amount.String(),
	}
}