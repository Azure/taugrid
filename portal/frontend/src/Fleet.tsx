// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { PageTitle } from './components';
import { InfiniBandFleet } from './InfiniBand';

export function Fleet() {
  return <><PageTitle title="Fleet">Node health, schedulable GPU capacity, active GPU assignments, utilization, and InfiniBand capability by Unbounded site.</PageTitle>
    <InfiniBandFleet/>
  </>;
}
