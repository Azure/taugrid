// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const ACTIONS = ["Short", "Flat", "Long"];
const ACTION_POSITIONS = [-1, 0, 1];
const inferenceSteps = ["features", "worker", "policy", "render"];
const EPISODE_LENGTH = 128;
const HISTORY_LENGTH = 96;

const clamp = (value, minimum, maximum) => Math.min(maximum, Math.max(minimum, value));

const sampleNormal = () => {
  const first = Math.max(Math.random(), Number.EPSILON);
  const second = Math.random();
  return Math.sqrt(-2 * Math.log(first)) * Math.cos(2 * Math.PI * second);
};

const chooseRegime = () => {
  const value = Math.random();
  return value < 0.3 ? -1 : value < 0.7 ? 0 : 1;
};

const createWorkerClient = () => {
  const worker = new Worker(new URL("./tau-market-policy-worker.js", import.meta.url), {
    type: "module",
  });
  const pendingRequests = new Map();
  let failure = null;
  let requestId = 0;

  const rejectPending = (error) => {
    if (failure) {
      return;
    }

    failure = error;
    worker.terminate();

    for (const pending of pendingRequests.values()) {
      window.clearTimeout(pending.timeout);
      pending.reject(error);
    }
    pendingRequests.clear();
  };

  worker.addEventListener("error", (event) => {
    event.preventDefault();
    rejectPending(new Error("The market policy Worker failed to load."));
  });
  worker.addEventListener("messageerror", () => {
    rejectPending(new Error("The market policy Worker returned an unreadable response."));
  });

  worker.addEventListener("message", (event) => {
    const pending = pendingRequests.get(event.data.requestId);

    if (!pending) {
      return;
    }

    pendingRequests.delete(event.data.requestId);
    window.clearTimeout(pending.timeout);

    if (event.data.type === "error") {
      pending.reject(new Error(event.data.error));
    } else {
      pending.resolve(event.data);
    }
  });

  const request = (message) =>
    new Promise((resolve, reject) => {
      if (failure) {
        reject(failure);
        return;
      }

      const nextRequestId = requestId;
      requestId += 1;
      const timeout = window.setTimeout(() => {
        rejectPending(new Error("The market policy Worker did not respond."));
      }, 10000);
      pendingRequests.set(nextRequestId, { reject, resolve, timeout });

      try {
        worker.postMessage({ ...message, requestId: nextRequestId });
      } catch (error) {
        pendingRequests.delete(nextRequestId);
        window.clearTimeout(timeout);
        reject(error);
      }
    });

  return {
    infer: (input) => request({ input, type: "infer" }),
    initialize: (modelUrl) => request({ modelUrl, type: "initialize" }),
    terminate: () => rejectPending(new Error("The market policy Worker was closed.")),
  };
};

const initializeMarketDemo = async (demo) => {
  if (demo.dataset.marketInitialized === "true") {
    return;
  }
  demo.dataset.marketInitialized = "true";

  const canvas = demo.querySelector("[data-market-canvas]");
  const context = canvas.getContext("2d");
  const toggleButton = demo.querySelector("[data-market-toggle]");
  const resetButton = demo.querySelector("[data-market-reset]");
  const runtimeStatus = demo.querySelector("[data-market-runtime-status]");
  const phaseElement = demo.querySelector("[data-market-phase]");
  const decisionElement = demo.querySelector("[data-market-decision]");
  const statusElement = demo.querySelector("[data-market-status]");
  const valueElement = demo.querySelector("[data-market-value]");
  const signalTrack = demo.querySelector("[data-market-signal-track]");
  const signalFill = demo.querySelector("[data-market-signal-fill]");
  const actionsElement = demo.querySelector("[data-market-actions]");
  const parameterMetric = demo.querySelector("[data-market-parameters]");
  const accuracyMetric = demo.querySelector("[data-market-accuracy]");
  const trainingDeviceMetric = demo.querySelector("[data-market-training-device]");
  const latencyMetric = demo.querySelector("[data-market-latency]");
  const priceElement = demo.querySelector("[data-market-price]");
  const signalElement = demo.querySelector("[data-market-signal]");
  const volatilityElement = demo.querySelector("[data-market-volatility]");
  const inventoryElement = demo.querySelector("[data-market-inventory]");
  const rewardElement = demo.querySelector("[data-market-reward]");
  const totalRewardElement = demo.querySelector("[data-market-total-reward]");
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
  const stepElements = new Map(
    [...demo.querySelectorAll("[data-market-step]")].map((element) => [
      element.dataset.marketStep,
      element,
    ]),
  );
  let workerClient = null;
  let timer = null;
  let isReady = false;
  let isRunning = false;
  let isInferring = false;
  let state;

  const resetState = () => {
    state = {
      action: 1,
      cumulativeReward: 0,
      inventory: 0,
      lastAction: 0,
      phase: 0,
      price: 100,
      priceHistory: [100],
      regime: chooseRegime(),
      reward: 0,
      signal: 0,
      tick: 0,
      volatility: 0.03,
    };
  };

  const setInferenceStep = (activeStep, completeThrough = false) => {
    const activeIndex = inferenceSteps.indexOf(activeStep);

    for (const [step, element] of stepElements) {
      const stepIndex = inferenceSteps.indexOf(step);
      element.classList.toggle("is-active", step === activeStep && !completeThrough);
      element.classList.toggle(
        "is-complete",
        completeThrough ? stepIndex <= activeIndex : stepIndex < activeIndex,
      );
    }
  };

  const renderActions = (probabilities) => {
    actionsElement.replaceChildren();

    probabilities.forEach((probability, actionIndex) => {
      const item = document.createElement("li");
      const row = document.createElement("div");
      const label = document.createElement("span");
      const percentage = document.createElement("strong");
      const track = document.createElement("div");
      const fill = document.createElement("span");
      label.textContent = ACTIONS[actionIndex];
      percentage.textContent = `${(probability * 100).toFixed(1)}%`;
      row.append(label, percentage);
      track.className = "tau-policy-demo__action-track";
      fill.style.transform = `scaleX(${probability})`;
      track.append(fill);
      item.append(row, track);
      actionsElement.append(item);
    });
  };

  const renderFacts = () => {
    priceElement.textContent = state.price.toFixed(2);
    signalElement.textContent = state.signal.toFixed(3);
    signalElement.dataset.sign = state.signal > 0.01 ? "positive" : state.signal < -0.01 ? "negative" : "neutral";
    volatilityElement.textContent = state.volatility.toFixed(3);
    inventoryElement.textContent = state.inventory.toFixed(2);
    rewardElement.textContent = state.reward.toFixed(3);
    totalRewardElement.textContent = state.cumulativeReward.toFixed(3);
    const normalizedSignal = clamp(state.signal / 0.1, -1, 1);
    signalFill.style.transform = `scaleX(${Math.abs(normalizedSignal)})`;
    signalFill.dataset.sign = normalizedSignal >= 0 ? "positive" : "negative";
    signalTrack.setAttribute(
      "aria-label",
      `Current latent market signal ${state.signal.toFixed(3)}`,
    );
  };

  const drawChart = () => {
    const ratio = window.devicePixelRatio || 1;
    const width = Math.max(1, Math.round(canvas.clientWidth * ratio));
    const height = Math.max(1, Math.round(canvas.clientHeight * ratio));

    if (canvas.width !== width || canvas.height !== height) {
      canvas.width = width;
      canvas.height = height;
    }

    const styles = getComputedStyle(demo);
    const background = styles.getPropertyValue("--cp-bg").trim();
    const border = styles.getPropertyValue("--cp-border").trim();
    const textMuted = styles.getPropertyValue("--cp-text-muted").trim();
    const accent = styles.getPropertyValue("--cp-accent").trim();
    const success = styles.getPropertyValue("--cp-success").trim();
    const danger = styles.getPropertyValue("--cp-danger").trim();
    const padding = 30 * ratio;
    const chartWidth = width - padding * 2;
    const chartHeight = height - padding * 2;
    const minimumPrice = Math.min(...state.priceHistory);
    const maximumPrice = Math.max(...state.priceHistory);
    const priceRange = Math.max(maximumPrice - minimumPrice, 0.05);
    context.clearRect(0, 0, width, height);
    context.fillStyle = background;
    context.fillRect(0, 0, width, height);
    context.strokeStyle = border;
    context.lineWidth = ratio;

    for (let line = 0; line <= 4; line += 1) {
      const y = padding + (chartHeight * line) / 4;
      context.beginPath();
      context.moveTo(padding, y);
      context.lineTo(width - padding, y);
      context.stroke();
    }

    context.strokeStyle = accent;
    context.lineWidth = 2 * ratio;
    context.beginPath();
    state.priceHistory.forEach((price, index) => {
      const x =
        padding +
        (chartWidth * index) / Math.max(state.priceHistory.length - 1, 1);
      const y =
        padding +
        chartHeight -
        ((price - minimumPrice) / priceRange) * chartHeight;

      if (index === 0) {
        context.moveTo(x, y);
      } else {
        context.lineTo(x, y);
      }
    });
    context.stroke();

    const lastPrice = state.priceHistory.at(-1);
    const lastY =
      padding +
      chartHeight -
      ((lastPrice - minimumPrice) / priceRange) * chartHeight;
    context.fillStyle =
      state.action === 0 ? danger : state.action === 2 ? success : textMuted;
    context.beginPath();
    context.arc(width - padding, lastY, 5 * ratio, 0, Math.PI * 2);
    context.fill();
  };

  const getObservation = (microNoise) => [
    state.signal,
    state.volatility,
    state.inventory / 4,
    state.phase,
    Math.sin(2 * Math.PI * state.phase),
    Math.cos(2 * Math.PI * state.phase),
    state.lastAction,
    microNoise,
  ];

  const sampleEnvironment = () => {
    if (state.tick % 18 === 0) {
      state.regime = chooseRegime();
    }

    state.signal = 0.035 * state.regime + sampleNormal() * 0.03;
    state.volatility = 0.01 + Math.random() * 0.07;
    state.phase = (state.tick % EPISODE_LENGTH) / EPISODE_LENGTH;
    return sampleNormal() * 0.01;
  };

  const applyAction = (action) => {
    const position = ACTION_POSITIONS[action];
    const oracle =
      state.signal > 0.01 ? 1 : state.signal < -0.01 ? -1 : 0;
    const churn = Math.abs(position - state.lastAction);
    const realizedReturn = state.signal + sampleNormal() * state.volatility;
    const pnl = position * realizedReturn;
    const alignment =
      (position === oracle ? 1 : -0.35) * Math.abs(state.signal) * 10;
    const inventoryPenalty = 0.0025 * (state.inventory + position) ** 2;
    const churnPenalty = 0.001 * churn;
    state.reward = alignment + pnl - inventoryPenalty - churnPenalty;
    state.cumulativeReward += state.reward;
    state.inventory = clamp(0.92 * state.inventory + position, -4, 4);
    state.lastAction = position;
    state.action = action;
    state.price += realizedReturn;
    state.priceHistory.push(state.price);

    if (state.priceHistory.length > HISTORY_LENGTH) {
      state.priceHistory.shift();
    }

    state.tick += 1;
  };

  const step = async () => {
    if (!isReady || isInferring) {
      return;
    }

    isInferring = true;
    const microNoise = sampleEnvironment();
    setInferenceStep("features");
    await new Promise((resolve) => requestAnimationFrame(resolve));
    setInferenceStep("worker");

    try {
      const result = await workerClient.infer(getObservation(microNoise));
      setInferenceStep("policy");
      const selectedAction = result.probabilities.indexOf(
        Math.max(...result.probabilities),
      );
      valueElement.textContent = result.value.toFixed(3);
      latencyMetric.textContent =
        result.elapsedMs < 0.1 ? "<0.1 ms" : `${result.elapsedMs.toFixed(2)} ms`;
      renderActions(result.probabilities);
      applyAction(selectedAction);
      decisionElement.textContent = ACTIONS[selectedAction];
      statusElement.textContent =
        selectedAction === 0
          ? "The actor sees a negative latent return and reduces exposure."
          : selectedAction === 2
            ? "The actor sees a positive latent return and adds exposure."
            : "The actor keeps inventory flat while the signal is neutral.";
      phaseElement.textContent = isRunning ? "Running" : "Paused";
      runtimeStatus.textContent = "Market policy ready";
      setInferenceStep("render", true);
      renderFacts();
      drawChart();
      demo.dataset.tauState = "ready";
    } catch (error) {
      showFailure(error, "Inference failed");
    } finally {
      isInferring = false;
    }
  };

  const schedule = () => {
    if (!isRunning) {
      return;
    }

    void step().finally(() => {
      if (isRunning) {
        timer = window.setTimeout(schedule, 260);
      }
    });
  };

  const start = () => {
    if (!isReady || isRunning) {
      return;
    }

    isRunning = true;
    toggleButton.textContent = "Pause policy";
    phaseElement.textContent = "Running";
    schedule();
  };

  function stop() {
    isRunning = false;
    window.clearTimeout(timer);
    timer = null;
    toggleButton.textContent = "Run policy";
    phaseElement.textContent = "Paused";
  }

  const showFailure = (error, heading) => {
    stop();
    isReady = false;
    toggleButton.disabled = true;
    resetButton.disabled = true;
    demo.dataset.tauState = "error";
    runtimeStatus.textContent = "Policy unavailable";
    phaseElement.textContent = "Error";
    decisionElement.textContent = heading;
    statusElement.textContent =
      error instanceof Error ? error.message : "The market policy Worker could not run.";
    workerClient?.terminate();
  };

  const reset = () => {
    stop();
    resetState();
    renderFacts();
    drawChart();
    decisionElement.textContent = "Market reset";
    statusElement.textContent = "Run the policy to begin a new synthetic episode.";
    setInferenceStep("features");

    for (const element of stepElements.values()) {
      element.classList.remove("is-active", "is-complete");
    }

    if (reducedMotion.matches) {
      void step();
    } else {
      start();
    }
  };

  toggleButton.addEventListener("click", () => {
    if (isRunning) {
      stop();
    } else {
      start();
    }
  });
  resetButton.addEventListener("click", reset);
  new ResizeObserver(drawChart).observe(canvas);
  resetState();
  renderFacts();
  drawChart();

  try {
    workerClient = createWorkerClient();
    window.addEventListener("pagehide", (event) => {
      if (!event.persisted) {
        workerClient?.terminate();
      }
    });
    const initialized = await workerClient.initialize(
      new URL("../models/tau-market-policy.json", import.meta.url).href,
    );
    isReady = true;
    toggleButton.disabled = false;
    resetButton.disabled = false;
    parameterMetric.textContent = initialized.parameterCount.toLocaleString();
    accuracyMetric.textContent = `${(initialized.metrics.policyAccuracy * 100).toFixed(2)}%`;
    trainingDeviceMetric.textContent =
      initialized.training.gpuName || initialized.training.device.toUpperCase();
    runtimeStatus.textContent = "Market policy ready";
    phaseElement.textContent = "Ready";
    decisionElement.textContent = "Policy ready";
    statusElement.textContent = "Run the synthetic environment to inspect its actions.";

    if (reducedMotion.matches) {
      await step();
    } else {
      start();
    }
  } catch (error) {
    showFailure(error, "Model failed to load");
  }
};

document.querySelectorAll("[data-tau-market-demo]").forEach((demo) => {
  void initializeMarketDemo(demo);
});
