// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

let model = null;

const validateModel = (candidate) => {
  if (
    candidate?.format !== "taugrid-market-policy" ||
    candidate?.version !== 1 ||
    candidate?.architecture?.inputCount !== 8 ||
    candidate?.architecture?.hiddenCount !== 24 ||
    candidate?.architecture?.actionCount !== 3
  ) {
    throw new Error("Unsupported market policy format.");
  }

  return candidate;
};

const infer = (input) => {
  const { architecture, weights } = model;

  if (!Array.isArray(input) || input.length !== architecture.inputCount) {
    throw new Error(`Market policy expected ${architecture.inputCount} observation values.`);
  }

  const hidden = new Float64Array(architecture.hiddenCount);

  for (let hiddenIndex = 0; hiddenIndex < architecture.hiddenCount; hiddenIndex += 1) {
    let sum = weights.hiddenBias[hiddenIndex];
    const offset = hiddenIndex * architecture.inputCount;

    for (let inputIndex = 0; inputIndex < architecture.inputCount; inputIndex += 1) {
      sum += weights.inputWeights[offset + inputIndex] * input[inputIndex];
    }

    hidden[hiddenIndex] = Math.max(0, sum);
  }

  const logits = new Float64Array(architecture.actionCount);

  for (let actionIndex = 0; actionIndex < architecture.actionCount; actionIndex += 1) {
    let sum = weights.policyBias[actionIndex];
    const offset = actionIndex * architecture.hiddenCount;

    for (let hiddenIndex = 0; hiddenIndex < architecture.hiddenCount; hiddenIndex += 1) {
      sum += weights.policyWeights[offset + hiddenIndex] * hidden[hiddenIndex];
    }

    logits[actionIndex] = sum;
  }

  const maximumLogit = Math.max(...logits);
  const probabilities = Array.from(logits, (logit) => Math.exp(logit - maximumLogit));
  const totalProbability = probabilities.reduce((sum, probability) => sum + probability, 0);
  probabilities.forEach((probability, index) => {
    probabilities[index] = probability / totalProbability;
  });

  let value = weights.valueBias[0];

  for (let hiddenIndex = 0; hiddenIndex < architecture.hiddenCount; hiddenIndex += 1) {
    value += weights.valueWeights[hiddenIndex] * hidden[hiddenIndex];
  }

  return { probabilities, value };
};

self.addEventListener("message", async (event) => {
  const { input, modelUrl, requestId, type } = event.data;

  try {
    if (type === "initialize") {
      const response = await fetch(modelUrl);

      if (!response.ok) {
        throw new Error("Unable to load the market policy.");
      }

      model = validateModel(await response.json());
      const parameterCount = Object.values(model.weights).reduce(
        (sum, values) => sum + values.length,
        0,
      );
      self.postMessage({
        metrics: model.metrics,
        parameterCount,
        requestId,
        training: model.training,
        type: "initialized",
      });
      return;
    }

    if (type !== "infer" || !model) {
      throw new Error("Market policy Worker is not initialized.");
    }

    const startedAt = performance.now();
    const result = infer(input);
    self.postMessage({
      ...result,
      elapsedMs: performance.now() - startedAt,
      requestId,
      type: "result",
    });
  } catch (error) {
    self.postMessage({
      error: error instanceof Error ? error.message : "Market policy inference failed.",
      requestId,
      type: "error",
    });
  }
});
