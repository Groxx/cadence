# Rate-limiting in Cadence

There are a variety of limits within Cadence.  Generally speaking: check the code!  
Some select ones and the overall systems are / should be called out in this doc though.


## Implementations

### [common/quotas]

Most limits are implemented in [common/quotas], which defines a couple slightly-varying flavors
built around one core in-memory-only implementation: [golang.org/x/time/rate]

Within that, there are essentially four pieces of behavior worth being aware of:

1. Most limiters are re-configurable at runtime via [common/dynamicconfig]. 
   Any limit in use is re-checked every 60 seconds, and if changed, a _new ratelimiter_ is created with the new limit,
   so the existing state is lost. This will probably be changed to simply re-configure at some point, as that
   does not seem to be intended / desirable, just a historical artifact.
2. Essentially all limiters have burst == RPS, allowing them to consume up to one second worth of their quota
   in any given burst of requests.
3. Some limiters are "global" and some are not.  "Global" limiters divide their RPS limits by the number of hosts within
   their ring.  E.g. a limit of 100rps with 5 hosts will lead to each host independently creating a 20rps (and 20 burst)
   ratelimiter.  Changes to ring size or the configured limit will re-adjust exactly like #1.
4. Most user-visible quotas (and some/many internal ones) are _limits_, immediately rejecting traffic beyond a certain point.  There
   are many places where [golang.org/x/time/rate]'s ability to _delay_ traffic to _meet_ a quota is used though, e.g.
   inside queue processing this is used to smooth out the naturally-bursty processing from bulk DB operations.

### [common/quotas/global] (needs rename, "global" is confusing)

Since [common/quotas] defines an in-memory-only limiter, it has some restrictions.  First and foremost: it is not aware of
load differences between hosts (e.g. say one host receives 90% of the requests for a domain).

This is not always relevant (e.g. internally-routed traffic may co-locate traffic on purpose), but for many user-visible
and frontend-contacting purposes, a load-balance-aware limiter will meet expectations better.  So that's what this package
implements, ultimately wrapping [common/quotas].

At a very high level, this system works by having two kinds of hosts per ratelimit:
one or more "limiter" hosts which are using a key, and one "decider" host that aggregates load information from all limiters.  

- Limiters collect per-key usage metrics in memory
- Limiters periodically report their aggregated metrics to each key's decider
- Limiters receive per-key weight information from the decider, and adjust their limits to match
- If this fails or the decider reports it has insufficient data:
  - If _no_ weight information has been retrieved, behavior falls back to the "global" limit in [common/quotas]
  - If previous reports worked, previous weights will be retained for N reporting periods to give caches time to recover (e.g. due to losing a decider host)
    - After this period passes, behavior falls back to the "global" limit in [common/quotas] on the assumption that the global ratelimiter is wholly broken
  - The next periodic report may recover this, and resume normal load-balanced limiting

The exact details of the current / all available load-aware quotas will be in [common/loadquotas] as they are built and documented, but in
general they are expected to be:

- Asynchronously updated, pull-based, and batched to reduce RPC load (i.e. not per request)
- Dynamically configurable, and dynamically migrate-able between implementations (to support cache-warming, rollbacks, and verification before use)
  - All should have 4 stages: use-old, use-old/shadow-new, use-new/shadow-old, use-new.
- Pluggable, so Cadence operators can write their own algorithm using this pattern, without relying on Cadence to implement them
  - The "push info, get limiter result, update in-memory quotas" framework is baked in, but everything else should be replace-able, including RPC types (via `google/protobuf/any.proto`)
- Only available to Cadence servers using gRPC between instances, as we intend to deprecate TChannel+Thrift internally
  - Public APIs are separate from this, and will likely maintain TChannel/Thrift for longer to support older client libraries

Flexibility within those constraints will hopefully be able to use this package without re-implementing everything from scratch.

[golang.org/x/time/rate]: https://pkg.go.dev/golang.org/x/time/rate
[common/quotas]: ../common/quotas
[common/dynamicconfig]: ../common/dynamicconfig
[common/quotas/global]: ../common/quotas/global