import { Composition } from 'remotion';
import { FoxyIntro, FOXY_INTRO_TOTAL_FRAMES } from './FoxyIntro';

export const RemotionRoot: React.FC = () => (
  <>
    <Composition
      id="FoxyIntro"
      component={FoxyIntro}
      durationInFrames={FOXY_INTRO_TOTAL_FRAMES}
      fps={30}
      width={1920}
      height={1080}
    />
  </>
);
