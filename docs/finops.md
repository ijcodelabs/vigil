# AI costs

What your use of language models and GPUs costs, by service, by model and
by machine: vigil counts the usage, you give it the prices, and cost is
usage times price in any query, dashboard or alert.

## Tokens: who uses which model, and how much

Applications that call a language model and are instrumented with
OpenTelemetry describe each call in a span: the provider, the model, and
the tokens that went in and came out (the "GenAI" conventions; the
OpenTelemetry instrumentations for the OpenAI, Anthropic, Google, Bedrock
and other SDKs, and libraries such as OpenLLMetry, do this without code
changes). The agent counts every such span:

| Metric | Meaning | Labels |
|---|---|---|
| `genai_requests_total` | calls to a model | `service`, `provider`, `model`, `operation`, `status` |
| `genai_tokens_total` | tokens used | `service`, `provider`, `model`, `type`: `input`, `output`, `cache_read`, `cache_write`, `reasoning` |
| `genai_request_duration_seconds_sum`, `_count` | how long calls took | as `genai_requests_total` |

So this needs what [Services and traces](apm.md) needs: `otlp: {enabled:
true}` on the agents, and the application pointed at its agent. Every call
is counted whatever the trace sampling is. The calls also show as traces,
with the model as something the service depends on.

Claude Code is not counted here: it reports its own usage and cost
(`claude_code_token_usage_total`, `claude_code_cost_usage_total`), and
its spans name the model but carry no `gen_ai.usage.*` tokens, so counting
them too would show its calls twice.

`model` is the model that answered (`gen_ai.response.model`), which usually
carries a version date. How `input` relates to `cache_read` differs by
provider and library: some report cached tokens inside the input count,
some beside it. Check one request against your provider's console before
trusting totals to the cent.

## GPUs

Agents report GPUs by themselves; see [GPUs and AI machines](gpu.md). For
cost, what matters is `gpu_info` (which cards exist) and
`gpu_utilization_ratio` (how much of the time they worked).

## Prices

In the server's config. One currency of your choosing; nothing is built in,
because prices change and a wrong built-in price would be a wrong bill:

```yaml
costs:
  tokens:                       # per MILLION tokens
    - {model: claude-sonnet-4, input: 3.00, output: 15.00, cache_read: 0.30, cache_write: 3.75}
    - {model: gpt-4o-mini,     input: 0.15, output: 0.60}
    - {model: gpt-4o,          input: 2.50, output: 10.00}
  gpu_hours:                    # one GPU for one hour
    - {model: "NVIDIA A100", price: 3.20}
    - {model: "",            price: 1.00}   # every other card
```

(The numbers above are examples of the format, not current prices.)

An entry applies to every model whose name **starts with** its `model`, the
longest match winning: `gpt-4o` prices `gpt-4o-2024-08-06`, but
`gpt-4o-mini-…` goes to the `gpt-4o-mini` entry. A kind of token without a
price (say `cache_write`) is left out of cost rather than counted as free.
For GPUs, `model` is what the card calls itself (`gpu_info`), and an entry
with an empty model is the default. For machines you own, a price per hour
is the purchase and running cost spread over their life; for rented ones,
the rent.

The server turns the list into two series, refreshed every half minute, for
exactly the models in use:

| Metric | Meaning |
|---|---|
| `genai_token_price{model, type}` | price of one token |
| `gpu_hour_price{model}` | price of one such GPU for one hour |

## The AI costs dashboard

Comes with the server. Token cost in the shown time and at the current
rate per day, cost per call, cost per hour by model, cost by service and by
model, tokens per call; GPU cost per hour and in the shown time, **how much
of it was idle time**, and which GPUs idled most. Two tables list what is
in use but has **no price**, so nothing is silently missing from the sums.

## Queries and alerts

```
# token cost per service over the last day
sum by (service) (increase(genai_tokens_total[1d]) * on (model, type) group_left genai_token_price)

# average cost of one call, by model
sum by (model) (increase(genai_tokens_total[1h]) * on (model, type) group_left genai_token_price)
  / sum by (model) (increase(genai_requests_total[1h]))

# money spent on GPUs doing nothing, last 24 hours
sum((1 - avg_over_time(gpu_utilization_ratio[1d]))
    * on (agent, gpu) group_left (model) gpu_info
    * on (model) group_left gpu_hour_price) * 24
```

```yaml
groups:
  - name: ai-costs
    rules:
      - alert: TokenSpendHigh
        expr: sum(increase(genai_tokens_total[1d]) * on (model, type) group_left genai_token_price) > 200
        labels: {severity: warning}
        annotations:
          summary: 'Language models cost {{ $value | printf "%.0f" }} in the last 24 hours'
      - alert: TokenSpendJump
        expr: |
          sum by (service) (rate(genai_tokens_total[30m]) * on (model, type) group_left genai_token_price)
            > 4 * sum by (service) (rate(genai_tokens_total[1d] offset 30m) * on (model, type) group_left genai_token_price)
          and sum by (service) (increase(genai_requests_total[30m])) > 50
        for: 15m
        labels: {severity: warning}
        annotations:
          summary: '{{ $labels.service }} spends four times its usual rate on language models'
      - alert: ModelWithoutPrice
        expr: count by (model) (genai_tokens_total) unless on (model) genai_token_price
        for: 1h
        labels: {severity: info}
        annotations:
          summary: 'Model {{ $labels.model }} is in use but has no price in the server''s costs'
```

## What this is not

- It counts what **your applications report**. It does not read the
  providers' own usage and billing APIs, so usage from tools that are not
  instrumented (a notebook, a vendor's hosted agent) is not in it, and
  nothing reconciles the total with the invoice. That is the next step.
- Applications that send the OpenTelemetry *metric*
  `gen_ai.client.token.usage` instead of, or besides, spans have that
  stored too (as `gen_ai_client_token_usage_*`), but the dashboard and the
  price series use the span-derived `genai_*` metrics only.
- Prices are per model, not per provider or per account; discounts, batch
  prices and free tiers are not modelled.
- Changing prices needs a server restart, and applies from then on: past
  usage is always shown at the current price.
