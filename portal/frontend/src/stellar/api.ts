// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
export const STELLAR_API = '/api/v2/stellar';

export function stellarURL(endpoint: string, params: Record<string, string | number | boolean | undefined> = {}) {
  const query = new URLSearchParams();
  for (const [name, value] of Object.entries(params)) if (value !== undefined && value !== '') query.set(name, String(value));
  return `${STELLAR_API}/${endpoint}${query.size ? '?' + query : ''}`;
}
