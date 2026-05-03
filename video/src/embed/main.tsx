import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { FoxyIntroPlayer } from './PlayerApp';

if (typeof window !== 'undefined') {
  const here = new URL('.', document.baseURI).pathname.replace(/\/$/, '');
  (window as unknown as { remotion_staticBase?: string }).remotion_staticBase =
    here;
}

const root = document.getElementById('root');
if (root) {
  createRoot(root).render(
    <StrictMode>
      <FoxyIntroPlayer autoPlay loop controls clickToPlay />
    </StrictMode>,
  );
}
