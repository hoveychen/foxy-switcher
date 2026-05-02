import { Player, type PlayerRef } from '@remotion/player';
import { useRef } from 'react';
import { FoxyIntro, FOXY_INTRO_TOTAL_FRAMES } from './FoxyIntro';

export interface FoxyIntroPlayerProps {
  /** Width as CSS dimension or pixels. Aspect ratio is locked to 16/9. */
  width?: number | string;
  /** Auto-play once mounted. */
  autoPlay?: boolean;
  /** Loop after reaching the end. */
  loop?: boolean;
  /** Show native player controls. */
  controls?: boolean;
  /** Toggle play/pause on click anywhere on the player. */
  clickToPlay?: boolean;
  /** Allow space-key toggle when focused. */
  spaceKeyToPlayOrPause?: boolean;
}

export const FoxyIntroPlayer: React.FC<FoxyIntroPlayerProps> = ({
  width = '100%',
  autoPlay = false,
  loop = true,
  controls = true,
  clickToPlay = true,
  spaceKeyToPlayOrPause = true,
}) => {
  const ref = useRef<PlayerRef>(null);
  return (
    <Player
      ref={ref}
      component={FoxyIntro}
      durationInFrames={FOXY_INTRO_TOTAL_FRAMES}
      fps={30}
      compositionWidth={1920}
      compositionHeight={1080}
      style={{ width, aspectRatio: '16 / 9' }}
      controls={controls}
      autoPlay={autoPlay}
      loop={loop}
      clickToPlay={clickToPlay}
      spaceKeyToPlayOrPause={spaceKeyToPlayOrPause}
    />
  );
};
