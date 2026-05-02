import { Player, type PlayerRef } from '@remotion/player';
import { useRef } from 'react';
import { FoxyIntro, FOXY_INTRO_TOTAL_FRAMES } from '../FoxyIntro';

export interface FoxyIntroPlayerProps {
  /** Width in pixels or CSS dimension. Defaults to 100% of container. */
  width?: number | string;
  /** Auto-play once the player mounts. */
  autoPlay?: boolean;
  /** Loop after reaching the end. */
  loop?: boolean;
  /** Show native player controls (play/pause/scrubber). */
  controls?: boolean;
  /** Click anywhere on the video to toggle play/pause. */
  clickToPlay?: boolean;
  /** Allow keyboard space-to-toggle when focused. */
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
