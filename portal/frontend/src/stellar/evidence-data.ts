// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useScopedURL, useWorkspace } from '../data';
import { useSearchParams } from 'react-router-dom';

export function useEvidenceURL() {
  const scopedURL = useScopedURL();
  const { scope } = useWorkspace();
  const [params] = useSearchParams();
  return (path: string): string => {
    const url = new URL(scopedURL(path), window.location.origin);
    if (url.origin !== window.location.origin) return url.href;
    if (url.pathname.startsWith('/api/v2/stellar/')) {
      if (scope.source) url.searchParams.set('source', scope.source);
      const project = params.get('project');
      if (project && !url.searchParams.has('project')) url.searchParams.set('project', project);
    }
    return url.pathname + url.search + url.hash;
  };
}
