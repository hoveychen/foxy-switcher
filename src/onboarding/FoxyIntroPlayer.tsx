import { Player, type PlayerRef } from '@remotion/player';
import { useEffect, useRef } from 'react';
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
  /** Fires when playback reaches the final frame (only meaningful when loop=false). */
  onEnded?: () => void;
}

export const FoxyIntroPlayer: React.FC<FoxyIntroPlayerProps> = ({
  width = '100%',
  autoPlay = false,
  loop = true,
  controls = true,
  clickToPlay = true,
  spaceKeyToPlayOrPause = true,
  onEnded,
}) => {
  const ref = useRef<PlayerRef>(null);

  useEffect(() => {
    const player = ref.current;
    if (!player || !onEnded) return;
    const handler = () => onEnded();
    player.addEventListener('ended', handler);
    return () => player.removeEventListener('ended', handler);
  }, [onEnded]);

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
