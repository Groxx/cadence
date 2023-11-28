// The MIT License (MIT)

// Copyright (c) 2017-2020 Uber Technologies Inc.

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

/*
Package aggregator blah

TODO: deserialize types.Any data, maintain a small sliding window of data per key, report back to callers.

strategy might be:
  - keep 10 (configurable) reporting periods (10s, configurable) in a cyclic buffer
  - per non-zero + per host in buffer, compute num of total requests per host (weight allow vs block?)
  - next update request updates data, recomputes host's share of requests, returns

start simple but high mem?
  - keep per-host data for N periods
  - new reports go into newest bucket, are ignored
  - take last 3 periods, compute weighted average of total traffic for this host (50%, 25%, 25%) ignoring its own zeros (sum others across zeros)
  - return max(1/N hosts, weight) to always allow a minimum through.  pool will re-balance soon.
  - ... make sure already-allowed is tracked so others do not exceed?

how do I handle "burst -> new host -> burst -> new host -> burst"?
  - across N periods, this would say "3 hosts each used 1/3rd" and the new 4th host gets a minimum of 1/N
  - next period, 4th host gets 50% as older 3 hosts had no activity (beyond time cutoff?)
  - next period, 4th host gets 75%
  - next period, 4th host gets 100%?

maybe:
  - compute total from previous N periods
  - compute proportion of latest update that this update would have consumed (can be >100%)
  - subtract already-used-this-period proportion from other updates
  - return whatever is left

this period: zeros are possible, as is 100%
next period: it'll get whatever proportion it contributed to last time

bah!  where do limits get considered, not just proportion?

eeehhhh... see what muttley does.  that can probably be copied directly.
*/
package aggregator
