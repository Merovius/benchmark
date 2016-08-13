# benchmark

This repository contains various programs to determine RobustIRC’s
performance.

## cmd/robustirc-loadtest

robustirc-loadtest is an orchestration tool for running a loadtest of
RobustIRC on [GCP (Google Cloud
Platform)](https://en.wikipedia.org/wiki/Google_Cloud_Platform). Using
GCP is a simple way to ensure that loadtest results are reproducible.

Refer to deployments/cluster.yaml for details on the resources which
are brought up as part of a loadtest. At the time of writing, the
resources are 3 100 GB Persistent Disk SSD volumes and 3
`n1-standard-8` Google Compute Engine VMs in a Google Container Engine
cluster.

Refer to [Google Compute Engine
Pricing](https://cloud.google.com/compute/pricing) for the costs
associated with these resources. At the time of writing, assuming a
loadtest takes 10 minutes, the pricing sums up to $0.18 USD for the 3
`n1-standard-8` VMs, with the Persistent Disk costs being negligible.

As prerequisite, please make sure to work through the “Before you
begin” sections of the following quickstart guides:

* [Compute Engine](https://cloud.google.com/compute/docs/quickstart-linux#before-you-begin)
* [Container Engine](https://cloud.google.com/container-engine/docs/quickstart#before-you-begin)
* [Deployment Manager](https://cloud.google.com/deployment-manager/quickstart#before-you-begin)

Usage:
```
$ go get -u github.com/robustirc/benchmark/cmd/robustirc-loadtest
$ cd $GOPATH/src/github.com/robustirc/benchmark
$ robustirc-loadtest
[…]
2016/08/13 13:50:46 sent 2016, recv 2004, (last 10s) min = 1841, max = 2011, spread = 170
2016/08/13 13:50:47 sent 2039, recv 1963, (last 10s) min = 1841, max = 2011, spread = 170
2016/08/13 13:50:48 converged! spread is < 10%
2016/08/13 13:50:48 Giving Prometheus another 15s (scrape_interval)
2016/08/13 13:51:04 RobustIRC dashboard snapshot stored at https://snapshot.raintank.io/dashboard/snapshot/V3n7wxutEooAOu4N6e0k6vQpmblwJyYj
2016/08/13 13:51:04 RobustIRC dashboard snapshot stored at https://snapshot.raintank.io/dashboard/snapshot/GQqYuaNO1BQc0Onw8DoIi7deXw9Dwqa0
```

You can call `robustirc-loadtest` as often as you wish. Each run will
recompile your local working copies of
[robustirc/robustirc](https://github.com/robustirc/robustirc/),
[robustirc/benchmark](https://github.com/robustirc/benchmark/) and
[robustirc/bridge](https://github.com/robustirc/bridge/) (currently
unused in a loadtest), restart the network on GCP and run another
test.

Once you’re done, be sure to cleanup to avoid paying for unused resources:
```
$ robustirc-loadtest -cleanup
```