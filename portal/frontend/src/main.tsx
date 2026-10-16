// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { QueryClientProvider } from '@tanstack/react-query';
import { App } from './App';
import { createPortalQueryClient } from './data';
import './styles.css';

const client = createPortalQueryClient();
createRoot(document.getElementById('root')!).render(
  <StrictMode><QueryClientProvider client={client}><BrowserRouter useTransitions={false}><App/></BrowserRouter></QueryClientProvider></StrictMode>,
);
