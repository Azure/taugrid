// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { PageTitle } from './components';
import { InfiniBandFleet } from './InfiniBand';

export function Fleet() {
  return <><PageTitle title="Fleet">Capacity, utilization, health, and InfiniBand evidence in one site-aware view.</PageTitle>
    <InfiniBandFleet/>
  </>;
}
