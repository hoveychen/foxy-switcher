import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { FoxyIntroPlayer } from './PlayerApp';

const Page: React.FC = () => (
  <main
    style={{
      minHeight: '100vh',
      display: 'flex',
      flexDirection: 'column',
      alignItems: 'center',
      justifyContent: 'center',
      gap: 24,
      padding: 40,
      background: '#E5DCCE',
      fontFamily:
        '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif',
      color: '#1A1A1A',
    }}
  >
    <h1
      style={{
        fontFamily: 'Fraunces, Georgia, serif',
        fontSize: 'clamp(36px, 5vw, 64px)',
        margin: 0,
        letterSpacing: '-0.02em',
      }}
    >
      Foxy Switcher
    </h1>
    <p style={{ margin: 0, opacity: 0.7, fontSize: 18 }}>
      An account pool for Claude Code.
    </p>
    <div style={{ width: 'min(1280px, 92vw)' }}>
      <FoxyIntroPlayer autoPlay loop controls clickToPlay />
    </div>
    <a
      href="https://github.com/hoveychen/foxy-switcher"
      style={{
        fontFamily: 'JetBrains Mono, ui-monospace, monospace',
        fontSize: 14,
        color: '#5C5A55',
        textDecoration: 'none',
      }}
    >
      github.com/hoveychen/foxy-switcher
    </a>
  </main>
);

const root = document.getElementById('root');
if (root) {
  createRoot(root).render(
    <StrictMode>
      <Page />
    </StrictMode>,
  );
}
