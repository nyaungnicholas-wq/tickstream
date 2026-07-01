# The Stoikov Micro-Price — explanation (v1 ships this as *documentation*, not code)

> **Scope honesty up front:** TickStream v1 computes **order-book imbalance**
> and the **size-weighted mid** LIVE. The micro-price below is *explained* so
> its owner can reason and talk about it; the offline fit → live `g(I,S)`
> lookup is an explicit **stretch goal (M6)** and has **not** been built.

## Why the weighted mid isn't enough

The size-weighted mid,

```
wmid = (Pb·Qa + Pa·Qb) / (Qa + Qb)
```

leans the fair-value estimate toward the heavy side of the book, which is
directionally right — but it is **not a martingale**. Stoikov's own
motivating example: if someone merely *cancels* part of the best ask, `Qa`
drops and the weighted mid *falls*, even though nothing about future prices
got more bearish. A "fair price" that reacts wrongly to cancellations is a
flawed predictor. That flaw is the entire reason the micro-price exists.

## Definition

The micro-price is the limit of expected future mid-prices at successive
mid-change times:

```
P_micro = lim(n→∞) E[ M(τₙ) | state now ]
```

Stoikov shows it decomposes as the current mid plus an adjustment that
depends **only** on the top-of-book imbalance `I = Qb/(Qb+Qa)` and the
half-spread `S = (Pa − Pb)/2`:

```
P_micro = M + g(I, S)
```

## The absorbing-Markov-chain fit (done OFFLINE, on captured data)

1. **Discretize the state.** Bucket `I` into `n_imb` quantile buckets
   (e.g. pandas `qcut`) and `S` into `n_spread` tick values. Mid-price moves
   are discretized to `K = [-2, -1, +1, +2]` ticks.
2. **Estimate transition matrices** from the captured tick data:
   - `Q` — transitions among *no-mid-move* (transient) states;
   - `R1` — transitions into an absorbing mid-move of size `k ∈ K`;
   - `R2` — transitions into a mid-move that lands in a new imbalance state.
3. **Solve for the expected cumulative adjustment:**

```
G1 = (I − Q)⁻¹ · R1 · K          # expected FIRST mid-move from each state
B  = (I − Q)⁻¹ · R2              # where you land after that move
G* = (I − B)⁻¹ · G1              # = G1 + B·G1 + B²·G1 + …
```

   The Neumann series converges fast in practice (~6 terms). `G*[state]`
   *is* `g(I,S)`.
4. **Serve it live:** export `g(I,S)` as a small lookup table; the live
   engine indexes it with the current imbalance bucket and spread and adds it
   to the mid. (This is the M6 stretch — numpy/pandas work outside this Go
   project.)

## Properties Stoikov proves

- **Martingale by construction** — it cannot react wrongly to cancellations
  the way the weighted mid does.
- **Bounded between bid and ask.**
- **Better short-horizon predictor** of future mids than either the mid or
  the weighted mid (demonstrated on BAC/CVX in the paper).

## Cheat-sheet

```
cheapest ──────────────────────────────▶ most accurate
   mid        <        wmid       <       micro-price
 (Pb+Pa)/2      imbalance-aware        martingale, fitted
                 but not a martingale   offline per symbol
```

## Reference

Sasha Stoikov, *The Micro-Price: A High-Frequency Estimator of Future
Prices* (2017). https://papers.ssrn.com/sol3/papers.cfm?abstract_id=2970694 —
reference implementation: https://github.com/sstoikov/microprice
