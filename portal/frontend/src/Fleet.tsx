// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { PageTitle } from './components';
import { InfiniBandFleet } from './InfiniBand';

export function Fleet() {
  return <><PageTitle title="Fleet">GPU capacity, utilization, health, and point-in-time two-GPU inter-node RDMA validation by Unbounded site. A run covers only recorded GPUs, not the fleet or multi-site distributed training.</PageTitle>
    <InfiniBandFleet/>
  </>;
}
