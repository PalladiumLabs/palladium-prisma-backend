import express from "express";
import { ethers } from "ethers";
import fs from "fs";
import cors from "cors";

// Load config.json
const config = JSON.parse(fs.readFileSync("./config.json", "utf8"));
/**
 * Optional decimals config to avoid hardcoding assumptions.
 * Adjust if your contracts use different scales.
 */
const DECIMALS = {
  COLL: config.COLL_DECIMALS ?? 8,     // e.g., SSS token has 8 decimals
  DEBT: config.DEBT_DECIMALS ?? 18,    // e.g., sss has 18 decimals
  PRICE: config.PRICE_DECIMALS ?? 8,   // PriceFeed TARGET_DIGITS, common = 8
};

// Provider
const provider = new ethers.JsonRpcProvider(config.RPC_URL);

// Load ABIs (do NOT mutate them)
const troveManagerAbi = JSON.parse(fs.readFileSync("./abi/TroveManager.json", "utf8"));
const priceFeedAbi = JSON.parse(fs.readFileSync("./abi/PriceFeed.json", "utf8"));

// Contract instances
const troveManager = new ethers.Contract(config.TROVE_MANAGER, troveManagerAbi, provider);
const priceFeed = new ethers.Contract(config.PRICE_FEED, priceFeedAbi, provider);

// Express app
const app = express();
const PORT = 3000;

app.use(cors());

// Helpers
const n = (x) => Number(x);
const fmt = (val, decimals) => n(ethers.formatUnits(val, decimals));

/**
 * Read price safely:
 *  - First try a static call to fetchPrice(_token).
 *  - If it reverts with PriceFeed__FeedFrozenError, mark frozen and fall back to priceRecords(_token).
 */
async function readPriceSafe(token) {
  let feedFrozen = false;
  let feedError = null;
  let priceRaw = 0n;
  let lastUpdated = 0;

  // Try staticCall on nonpayable method
  try {
    const fetchPriceFn = priceFeed.getFunction("fetchPrice");
    priceRaw = await fetchPriceFn.staticCall(token);
  } catch (err) {
    // Detect custom error name in ethers v6
    const errName =
      err?.data?.errorName ||
      err?.errorName ||
      (typeof err?.shortMessage === "string" && err.shortMessage.includes("PriceFeed__FeedFrozenError")
        ? "PriceFeed__FeedFrozenError"
        : undefined);

    if (errName === "PriceFeed__FeedFrozenError") {
      feedFrozen = true;
      feedError = "Feed is frozen";
    } else {
      feedError = err?.reason || err?.shortMessage || "Unknown price feed error";
    }
  }

  // Fallback to cached record when frozen or fetchPrice failed
  if (feedFrozen || priceRaw === 0n) {
    try {
      const rec = await priceFeed.priceRecords(token);
      // rec.scaledPrice is uint96
      if (rec?.scaledPrice && rec.scaledPrice !== 0n) {
        priceRaw = rec.scaledPrice;
        lastUpdated = Number(rec.lastUpdated || rec.timestamp || 0);
      }
    } catch (_e) {
      // ignore, we'll return 0 price if nothing available
    }
  }

  return {
    priceRaw,
    price: n(ethers.formatUnits(priceRaw, DECIMALS.PRICE)),
    feedFrozen,
    feedError,
    lastUpdated,
  };
}

/**
 * Optional: read oracle status flags to expose more diagnostics
 */
async function readOracleStatus(token) {
  try {
    const rec = await priceFeed.oracleRecords(token);
    return {
      isFeedWorking: Boolean(rec?.isFeedWorking),
      heartbeat: Number(rec?.heartbeat ?? 0),
      decimals: Number(rec?.decimals ?? 0),
      isEthIndexed: Boolean(rec?.isEthIndexed),
      chainLinkOracle: rec?.chainLinkOracle ?? ethers.ZeroAddress,
    };
  } catch {
    return null;
  }
}

app.get("/metrics", async (req, res) => {
  try {
    const token = config.ASSET_ADDRESS || "0x92a68f6de3bA732a13a0dDee7d5Ee77b2b3Bb63f";

    // Read price safely
    const { price, priceRaw, feedFrozen, feedError } = await readPriceSafe(token);

    // Contracts
    const borrowOpsAbi = JSON.parse(fs.readFileSync("./abi/BorrowOperations.json", "utf8"));
    const borrowOps = new ethers.Contract(config.BORROWER_OPERATIONS, borrowOpsAbi, provider);

    // Fetch system values
    const [
      totalCollRaw,
      totalDebtRaw,
      MCRRaw,
      CCRRaw,
      minNetDebtRaw,
      debtGasCompRaw,
      borrowRateRaw,
    ] = await Promise.all([
      troveManager.getEntireSystemColl(),
      troveManager.getEntireSystemDebt(),
      troveManager.MCR(),
      troveManager.CCR(),
      borrowOps.minNetDebt(),
      borrowOps.DEBT_GAS_COMPENSATION(),
      troveManager.getBorrowingRate(),
    ]);

    const totalcoll = fmt(totalCollRaw, DECIMALS.COLL); // token units
    const totaldebt = fmt(totalDebtRaw, DECIMALS.DEBT); // stablecoin units

    // Collateral value in USD: (tokens * priceUSD)
    const collUsd = totalcoll * price;

    // TCR = total collateral USD / total debt USD
    const TCR = totaldebt > 0 ? (collUsd / totaldebt) : 0;

    // Format ratios (scale by 1e18 → number like 1.1)
    const MCR = n(ethers.formatUnits(MCRRaw, 18));
    const CCR = n(ethers.formatUnits(CCRRaw, 18));

    // Convert new values
    const minDebt = fmt(minNetDebtRaw, DECIMALS.DEBT);
    const LR = fmt(debtGasCompRaw, DECIMALS.DEBT);
    const borrowRate = n(ethers.formatUnits(borrowRateRaw, 18)); // usually annualized %

    const data = [
      {
        _id: Date.now().toString(),
        metrics: [
          {
            token: config.SYMBOL || "pBTC",
            price,
            TCR: ethers.formatEther(Number(TCR).toString()),
            MCR,
            CCR,
            minDebt,
            LR,
            borrowRate,
            totalcoll,
            totaldebt,
            maxMint: 100000000,
          },
        ],
        pricePUSD: 1,
        priceyPUSD: 1.00503370738977,
        stakedPUSD: 8902.55020135273,
      },
    ];

    res.json(data);
  } catch (err) {
    console.error("Error in /metrics:", err);
    res.status(500).json({ error: err.message || String(err) });
  }
});



// Extra diagnostics: peek into oracle + price records
app.get("/debug/oracle", async (req, res) => {
  try {
    const token = config.ASSET_ADDRESS || "0x92a68f6de3bA732a13a0dDee7d5Ee77b2b3Bb63f";
    const [status, priceRec] = await Promise.all([
      readOracleStatus(token),
      priceFeed.priceRecords(token),
    ]);
    res.json({
      token,
      oracleStatus: status,
      priceRecord: {
        scaledPrice: priceRec?.scaledPrice?.toString?.() ?? "0",
        timestamp: Number(priceRec?.timestamp ?? 0),
        lastUpdated: Number(priceRec?.lastUpdated ?? 0),
        roundId: priceRec?.roundId ? priceRec.roundId.toString() : "0",
      },
    });
  } catch (err) {
    res.status(500).json({ error: err.message || String(err) });
  }
});

app.listen(PORT, () => {
  console.log(`🚀 Server running on http://localhost:${PORT}`);
});
